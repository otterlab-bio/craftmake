# craftmake action + Google Colab：统一架构 TDD

> TDD 在本文中指 Technical Design Document；交付过程同时采用 Test-Driven Development 的 red → green → refactor 垂直切片。

- 状态：Phase 2 complete + Colab 真实云端端到端打通 + Google Drive 自动化挂载完成（内置 Colab OAuth client + colaboratory scope + 凭据接入 + assign 协议对齐 + RFC 6455 TLS WebSocket 对接 Jupyter kernel channels + 412 TooManyAssignments 自动清理 + credentials-propagation 凭据传播 + colab_request/input_reply 内核握手 + bootstrap 自动挂载 + colab drive mount --authorize 授权流 + 真实 Google Cloud Colab 实例执行成功）
- 基线：craftmake Colab branch，现有 Go CLI / compiler / scheduler / backend / store 架构
- 目标：审查已批准计划，修正与现有代码和 Colab 运行模型不一致的假设，形成可执行的设计与测试交付方案

## 1. 审查结论

总体方向正确：action 应作为配置适配器，colab 应作为 Backend 实现，所有路径汇入同一条 compiler → scheduler → store 执行链。但原计划不能直接实现，必须先修正以下问题：

1. **现有 Backend 没有 run 生命周期**。RunSubmission 是按 submission 调用的，而计划要求一个 runtime 覆盖整次 run；必须增加可选的 RunLifecycle seam，并由 Scheduler 保证 Begin/End 只调用一次。
2. **现有 scheduler 允许多个 submission 并行**。单个 Jupyter kernel 不能并行执行；不能只在文档中说 workers=1，必须在 Colab backend/factory 和 Scheduler 入口双重强制。
3. **Colab 没有通用的“提交 ipynb 并执行”REST API**。可实现的语义是：生成可审计的 ipynb artifact，把其 code cells 通过 Jupyter kernel protocol 执行；ipynb 是任务封装和记录格式，不应伪装成 Colab 原生提交 API。
4. **Drive API 与 Drive mount 是两套能力**。Drive API 的 OAuth scope 不等于 runtime 内 `drive.mount()` 的预授权；无头 kernel 中直接调用 `drive.mount()` 可能阻塞等待用户。自动化必须单独建模 mount authorization/preflight。
5. **TaskManifest 的路径是本地路径**。不能直接把本地 WorkDirectory、Inputs、Outputs 原样交给 Colab。必须有显式的 RemoteWorkspaceMapping；否则现有绝对路径工作流无法在 Colab 上正确运行。
6. **远端日志必须回填本地路径**。现有 report/logs 依赖 StepResult 的 stdout/stderr 文件；只回传 TaskResult JSON 而不物化日志，会破坏现有观测链。
7. **Colab runtime 规格不能简单写成任意 T4/A100 枚举**。应通过 runtime specs/user-info 做能力校验，并使用 Colab 实际的 variant/accelerator/shape/runtime version 模型。
8. **action schema 需要显式 discriminator**。不能仅依赖 `version: 1` 猜测文件类型；建议使用 `schema_version: craftmake.action/v1`，避免与现有 WorkflowSpec 混淆。
9. **Colab public API v2 不能作为默认生产依赖**。参考实现显示它仍受实验开关/allowlist 限制；生产实现必须把 Colab v1/internal control-plane 封装在适配器中，并保留 live smoke test。
10. **单 OAuth 不能被视为已解决**。Colab scope、Drive API scope、runtime mount authorization 必须分别建模；若 Google client policy 不允许合并，使用 split credential store。
11. **Drive 作为持久化主工作区不等于所有 IO 都走 Drive FUSE**。保持 Drive 为逻辑 durable root，同时把高频临时文件放到 runtime `/content` scratch，并在 attempt/TaskResult 边界 checkpoint 到 Drive。
12. **ipynb 与 `%%bash` 是用户可观察 artifact/执行格式，但不是交互式终端抽象**。v1 明确不支持 TTY、跨 cell shell cwd/export 持久化和后台进程；需要交互式 shell 时另开 terminal module，不混入任务契约。

审查后的原则：**一个 PlanLoader、一个 RunEngine、一个 Backend interface、一个 Store；action 只新增输入适配器，colab 只新增执行适配器。**

## 2. 范围与非目标

### 2.1 本期范围

- `craftmake action list`、`action plan`、`action run`。
- cwd 下扁平 `.craftmake/*.yaml` 发现。
- 自包含 `craftmake.action/v1` schema。
- `action plan/run <name> --config <path>` 运行位于 `.craftmake/<name>.yaml` 的既有 `spec.WorkflowSpec`。
- 现有 `run`/`resume` 使用 `colab` backend。
- 一个 run 一个 Colab runtime，kernel 串行执行。
- 每个 Task 一个 `.ipynb` artifact，step 对应一个 `%%bash` cell。
- Drive 作为远端持久化工作区，任务失败/超时后可在新 runtime 中 resume。
- TaskResult、StepResult、日志、输出校验与现有 scheduler/store/report 契约兼容。

### 2.2 非目标

- v1 不实现 push/schedule/webhook 触发器。
- v1 不实现多 runtime pool 或 Colab 内并行任务。
- v1 不实现任意 Python notebook 交互式 UI；任务仍以 shell step 为主。
- v1 不保证任意包含本地绝对路径的现有 workflow 可以无配置迁移到 Colab。
- v1 不依赖 colab-cli 作为运行时 subprocess；它只作为协议和行为参考。

### 2.3 源码空间与状态空间

`.craftmake/*.yaml` 只属于源代码命名空间；SQLite、controller log、run artifacts 和临时 attempt 不得与 action YAML 混放。默认布局为：

    .craftmake/                    # source-controlled action definitions
    .craftmake/state/              # local state.sqlite, controller logs, runs
    .craftmake/state/runs/<run-id>/

`--state-dir` 仍可覆盖默认状态目录；既有 `workflow/.craftmake` 项目在兼容模式下保持可读，但新 action 默认使用 `.craftmake/state`，并生成/提示 `.craftmake/.gitignore`。持久化 run 记录必须同时保存 `LoaderKind`、`WorkflowPath`、可选 `ConfigPath`、source digest 和 backend name。

## 2.4 参数来源契约

所有可渲染字段统一使用带来源前缀的表达式；来源不得隐式混用：

- `${{ args.NAME }}`：命令行显式传入的参数。action/v1 中通过 `--arg NAME=VALUE` 传入；现有 `--input` 保留为兼容别名。参数必须在 action 的 `inputs` 声明中存在，默认值和 required 校验仍由 action loader 负责。
- `${{ env.NAME }}`：进程启动时采集的只读环境快照；action 顶层 `env` 经过渲染后合并进该命名空间，供后续 job/step 使用。环境变量不通过命令行参数覆盖。
- `${{ config.NAME }}`：既有 config-driven loader/compiler 的配置上下文，保持现状；不把 `args` 或 `env` 静默写入 config。配置文件路径、digest 和原始配置仍由既有流程管理。
- `${{ inputs.NAME }}`：`args` 的兼容别名，仅为已有 action/v1 文件提供迁移期兼容；新文件应使用 `args`。

优先级只发生在同一来源内部：命令行 args 覆盖该参数的 default，action env 覆盖同名进程环境变量；`config` 不参与跨来源覆盖。认证 token 不得进入表达式快照、notebook、controller log 或 TaskResult。

## 2.5 Colab workspace 与 session auth 契约

- 本地项目目录是 source root；Colab/Drive 上的 durable root 是 remote root；runtime `/content` 仅作 scratch。
- run 开始时执行一次 `local → remote` 同步，默认排除 `.craftmake/state` 和 `.git`；run 结束时执行一次 `remote → local` 同步，用于取回输出和诊断日志。
- 同步通过 `WorkspaceSyncer` seam 实现；当前 `FileWorkspaceSyncer` 仅用于离线测试，真实实现可替换为 Drive API/FUSE 或受控传输器。接口区分 `SyncIn` 与 `SyncOut`，并可用 `WorkspaceManifest` 对文件路径、大小、权限和 SHA-256 做确定性校验。
- `craftmake.action/v1` 可选声明 `colab` 配置块，包含 session、auth config、drive/remote/scratch root、同步方向和排除项；该块只能表达运行意图，不承载 OAuth token。
- `~/.config/craftmake/colab-auth.json` 使用 `craftmake.colab-auth/v1`，按 `session_id` 保存 Colab credential reference、Drive credential reference、`drive_root` 与 `mount_path`，文件权限必须为 `0600`。
- `craftmake colab auth configure` 只写 credential references，不把 token 写入配置；`craftmake colab drive mount --session NAME` 校验指定 session 并生成 mount plan。真正的 runtime mount 由 Colab backend 在 `BeginRun` 自动读取该 session 配置并调用 `DriveMountPreflight`。
- Control-plane credential、Drive API credential、runtime mount authorization 是独立能力；没有真实 adapter 时 CLI 与 fake tests 不宣称已经完成线上挂载。

## 2.6 参考仓库抽取的 Colab v1 协议（googlecolab/colab-vscode + MurphyLo/colab-cli）

从 `googlecolab/colab-vscode` 的 `src/colab/client/v1` 与 `MurphyLo/colab-cli` 抽取，作为真实 adapter 的契约基线（可离线单测，不需真实账号）：

- 两个 base domain：`colabDomain = https://colab.research.google.com`（`/tun/m` 隧道）与 `colabGapiDomain = https://colab.pa.googleapis.com`（`/v1` 用户/代理）。
- assign：`GET /tun/m/assign?nbh=<websafe-base64>&variant=<VARIANT>&acc=<accel>&shape=<n>&version=<ver>`。若无已分配机器返回 `{acc,nbh,p,token,variant}`（`token` 为 XSRF token），需再 `POST` 相同 URL 并在 `X-Goog-Colab-Token` 头携带该 token，返回 `{endpoint,accel,variant,machineShape,runtimeProxyInfo:{token,url}}`。
- unassign：`GET /tun/m/unassign/<endpoint>` 返回 `{token}`，再 `POST` 携带 XSRF token。
- keep-alive：`GET /tun/m/<endpoint>/keep-alive/` 携带 `X-Colab-Tunnel: Google`。
- refresh proxy：`GET /v1/runtime-proxy-token?endpoint=<endpoint>&port=8080` 返回 `{token, tokenTtl:"Ns", url}`。
- user-info：`GET /v1/user-info`（可选 `get_ccu_consumption_info=true`）返回 `{subscriptionTier, eligibleAccelerators:[{variant,models}], ineligibleAccelerators}`。
- 认证：`Authorization: Bearer <access_token>`；401 时刷新并重试一次（colab-vscode createAuthMiddleware）。colab-cli 用 loopback OAuth + PKCE S256 + `access_type=offline&prompt=consent`，刷新 margin 为 5 分钟，`invalid_grant` 清 session。
- 隧道内 notebook 执行与文件读写经 runtime proxy url，并携带 `X-Colab-Runtime-Proxy-Token`。

`ColabServerClient`（assign/unassign/keep-alive/refresh-proxy/user-info）、`ProxyNotebookExecutor`、loopback PKCE OAuth、`TokenManager`（margin 刷新）已实现并离线可测；生产 domain/token 端点在接入真实账号前可配置替换。


## 3. 统一架构

```text
CLI (run/action/status/report/resume)
        │
        ▼
RunEngine
  ├── PlanLoaderRegistry
  │     ├── existing config + WorkflowSpec adapter
  │     └── craftmake.action/v1 adapter
  ├── compiler.Compile
  ├── BackendFactoryRegistry
  │     ├── local
  │     ├── slurm
  │     └── colab
  ├── scheduler.Scheduler
  └── store.Store / report / logs / resume

Colab backend 内部：
  RunLifecycle
    ├── ColabControlPlane（runtime assign/list/unassign/proxy token）
    ├── DriveMountBootstrap（runtime 内 mount preflight）
    ├── RemoteWorkspace（路径映射、Drive 文件传输）
    ├── NotebookBuilder（TaskManifest → ipynb）
    ├── KernelExecutor（Jupyter WS messages）
    ├── ResultDecoder（TaskResult sentinel）
    └── LogMaterializer（远端日志 → 本地 StepResult 路径）
```

### 3.1 深模块接口

#### PlanLoader

建议把当前 `internal/cli/root.go` 中的 `loadPlan` 逐步抽到 engine seam：

    type LoadRequest struct {
        WorkflowPath string
        ConfigPath   string
        ProjectDir   string
        StateDir     string
        Phase        string
        CatalogDir   string
        Inputs       map[string]string
    }

    type LoadedPlan struct {
        Plan          *compiler.Plan
        Kind          string
        WorkflowPath  string
        ConfigPath    string
        WorkflowDigest string
        ConfigDigest   string
        BackendName    string
        Execution      compiler.ExecutionContext
    }

    type PlanLoader interface {
        Load(context.Context, LoadRequest) (*LoadedPlan, error)
    }

规则：
- 既有配置种类仍由现有 adapters 负责加载 Context。
- action adapter 在 `schema_version: craftmake.action/v1` 时同时产出 Context 与内联 WorkflowSpec。
- `--config` 模式不走 action adapter；它把 `.craftmake/<name>.yaml` 作为显式 WorkflowPath，复用既有 config loader。
- catalog routing 仍属于顶层 `craftmake run`；不引入 action 的隐式 catalog 解析。

#### RunEngine

    type RunRequest struct {
        LoadedPlan    *LoadedPlan
        BackendName   string
        RunID         string
        MaxParallel   int
        MaxCores      int
        MaxMemory     int64
        Force         bool
    }

    type RunEngine interface {
        Run(context.Context, RunRequest) (RunResult, error)
    }

RunEngine 统一负责：backend factory、runtime path、store、digest、scheduler、command envelope。CLI 只负责参数解析和输出格式，不再复制 run wiring。

#### Backend lifecycle

保留现有 `backend.Backend` 接口作为 submission seam，并增加可选接口：

    type RunLifecycle interface {
        BeginRun(context.Context, RunContext) error
        EndRun(context.Context, RunOutcome) error
    }

Scheduler 在一次 `Run` 中：
1. 若 backend 实现 RunLifecycle，调用一次 BeginRun。
2. 执行现有 initializeTasks / submission rounds。
3. 无论成功、失败、取消，都在有界 cleanup context 中调用一次 EndRun。

这样 local/slurm 无需改变；colab 可以实现“一次 run 一个 runtime”的真实生命周期。若后续发现所有 backend 都需要生命周期，再将可选接口升级为正式接口。

## 4. Action schema 与 CLI

### 4.1 推荐 schema

使用显式 schema discriminator：

    schema_version: craftmake.action/v1
    name: build-model
    backend: colab              # local | slurm | colab
    inputs:
      target_genome:
        description: Target genome
        required: false
        default: hg38
    env:
      LOG_LEVEL: info
    colab:
      variant: GPU              # DEFAULT | GPU | TPU
      accelerator: T4           # optional; validated against runtime specs
      shape: standard            # standard | high-ram
      runtime_version: "2025.10"
      drive:
        mount_path: /content/drive
        root: MyDrive/craftmake
        path_map:
          /analysis: MyDrive/analysis
    jobs:
      build:
        needs: []
        resources:
          cores: 4
          memory: 16G
        env:
          BUILD_MODE: release
        steps:
          - id: compile
            name: Compile
            run: make BUILD_MODE="$BUILD_MODE"
            env:
              CFLAGS: -O3

规范：
- `schema_version` 必填且固定为 `craftmake.action/v1`。
- `name`、job id、step id 必须是安全组件；step 没有 id 时按稳定 index 生成。
- workflow/job/step 三层 env 合并，内层覆盖外层。
- `inputs` 有 default/required；CLI 使用重复 `--input key=value`，未知 key、缺失 required、重复冲突均报错。
- 支持 `${{ inputs.x }}`、`${{ env.X }}`、`${{ job.id }}`、`${{ run.id }}` 等有限标量表达式；禁止把 map/list 渲染进 shell 字符串。
- `$CRAFTMAKE_OUTPUT` 和 `$CRAFTMAKE_ENV` 是每个 step 的受控文件接口；只有白名单格式可被解析并传递到后续 step。

### 4.2 CLI 语义

    craftmake action list [--dir DIR]
    craftmake action plan NAME [--dir DIR] [--config CONFIG] [--input K=V ...]
    craftmake action run NAME [--dir DIR] [--config CONFIG] [--input K=V ...]
        [--backend local|slurm|colab] [--state-dir DIR] [--run-id ID]
        [--workers N] [--max-cores N] [--max-memory SIZE] [--force]

- 无 `--config`：NAME 必须是 `.craftmake/NAME.yaml` 的 action/v1 文件。
- 有 `--config`：NAME 必须是 `.craftmake/NAME.yaml` 的既有 WorkflowSpec；该文件作为显式 workflow path，config 提供 samples/species/paths/backend。
- action list 只发现一级目录中的 `*.yaml`/`*.yml`，并显示 detected type；不执行 config 加载。
- `action plan` 和 `action run` 共享同一个 PlanLoader/RunEngine。

## 5. Colab backend 设计

### 5.1 控制面：使用可替换的 Colab v1/internal adapter

不能把 `colaboratory.googleapis.com` 的 public API v2 当作默认生产依赖：参考实现注明该 API 尚未公开，可能需要实验开关或 allowlist。生产适配器优先封装参考项目实际使用的 v1/internal control-plane；endpoint 和 header 版本化集中在一个 package，未来 public v2 只能作为另一个 adapter。

Colab 参考实现显示 runtime 创建与 proxy 获取包含 assignment、runtime-proxy-token、keep-alive、credentials propagation 等流程；因此实现应隐藏在 `ColabControlPlane` 接口之后：

    type ColabControlPlane interface {
        ListSpecs(context.Context) ([]RuntimeSpec, error)
        Assign(context.Context, AssignRequest) (Assignment, error)
        RefreshProxy(context.Context, string, int) (ProxyInfo, error)
        KeepAlive(context.Context, string) error
        Unassign(context.Context, string) error
        UserInfo(context.Context) (UserInfo, error)
    }

具体 endpoint、header、XSRF token、XSSI 前缀和错误码全部封装在实现中，不能泄漏到 scheduler 或 action schema。当前参考实现涉及 `colab.research.google.com/tun/m/*` runtime assignment/keep-alive，以及 `colab.pa.googleapis.com/v1/*` user-info/assignment/proxy-token 等调用；这些地址必须通过配置/版本化 adapter 管理，不能散落在业务代码中。412/503/quota/denylisted 转换为可分类的 Go error。

参考：
- https://github.com/googlecolab/colab-vscode
- https://github.com/MurphyLo/colab-cli

### 5.2 认证

OAuth loopback 仍是主认证入口，但“Colab + Drive 单 OAuth”不再视为已确认要求。参考实现显示 Colab 官方 client 与完整 Drive scope 存在 client policy 限制；实现必须支持 split credential store，并在 live smoke test 前验证实际 client ID/scope。凭据存储在受限权限的 `~/.config/craftmake/colab-auth.json` 与可选 `~/.config/craftmake/drive-auth.json`，均支持 refresh token。TDD 明确区分：

1. **Colab control-plane token**：运行时分配、proxy token、quota。
2. **Drive API token**：本地上传/下载和可选结果回传。
3. **Drive mount authorization**：runtime 内 `drive.mount()` 的无头预授权。

第 3 项不能假定由第 1/2 项自动获得。必须提供 mount preflight：如果 runtime 不能无交互挂载，任务在执行前失败并提示一次性授权命令；不得让 kernel 永久阻塞。

建议命令：

    craftmake colab auth login
    craftmake colab drive-auth login
    craftmake doctor --backend colab

是否把 control-plane 与 Drive API 合并成一次 OAuth 是实现细节；mount authorization 必须作为独立状态检查保留。

### 5.3 Run 生命周期

Colab backend 实现 `RunLifecycle`：

- BeginRun：读取 Colab 配置，校验 runtime specs，创建一个 runtime，获取 proxy URL/token，启动 keep-alive，执行 kernel/Drive preflight。
- RunSubmission：在同一个 runtime 上按 task 顺序执行；内部 mutex 是第二道串行保护。
- EndRun：停止 keep-alive，按策略 unassign runtime；失败时保留足够 metadata 供诊断。
- Scheduler/RunEngine 对 colab 将 MaxParallel 强制为 1；若用户显式传入大于 1，输出 warning 或直接 usage error，采用一种一致策略。建议直接 error，避免错误的并发预期。

Run metadata 记录在本地 state/submission metadata 中：runtime endpoint、remote workspace root、runtime version、notebook hashes、last heartbeat；不持久化 bearer/proxy secret。

### 5.4 RemoteWorkspace：路径和持久化

定义稳定远端根目录：

    /content/drive/<drive-root>/runs/<run-id>/tasks/<task-id>/attempt-<n>/

不要修改原始 TaskManifest 的本地路径；生成一个 backend-private mapping：

    type RemoteTaskMapping struct {
        WorkDirectory string
        TempDirectory string
        RuntimeDirectory string
        Inputs map[string]string
        Outputs map[string]string
    }

规则：
- action 的相对路径相对 RemoteWorkDirectory 解析。
- 既有 workflow 的绝对路径只有在 `colab.drive.path_map` 显式映射后才允许使用。
- 映射必须在执行前做 allowlist/escape 校验，禁止 `..` 穿越 drive root。
- 输入通过 Drive API 或预存在 Drive 的路径进入远端；声明的 durable inputs/outputs 写入 Drive mount，利用 Drive 的同步持久化。
- 高频临时文件、编译缓存和 kernel 临时文件优先使用 `/content/craftmake/...`；任务完成或 checkpoint 时将必须恢复的内容同步到 Drive。`CRAFTMAKE_WORK` 默认指向 Drive durable work，`CRAFTMAKE_TEMP` 默认指向 `/content` scratch。
- Drive sync 后做 bounded settle/retry，再做输出校验。

### 5.5 Notebook artifact 与 kernel execution

生成 notebook 不等于调用 Colab 的“notebook submit API”。每个 Task 产生两个结果：
1. 可审计 `.ipynb` artifact，写入本地 attempt runtime directory，并复制到 Drive task directory。
2. 由 KernelExecutor 逐 cell 执行的 execution stream。

Notebook cells：
- bootstrap Python：预检 mount、创建远端工作目录、写入安全 env、记录 task metadata。
- 每个 step 一个 `%%bash` cell：命令前置 `set -uo pipefail`，通过 trap/状态文件记录 exit code、开始/结束时间，输出写到远端 stdout/stderr 文件。v1 将每个 step 视为独立 shell invocation，与现有 local runtime 语义一致；不承诺 `cd`/`export`/后台进程跨 cell 持久化，也不支持交互式 TTY。
- finalizer Python：读取 step 状态、做 output validation、生成带 sentinel 的 TaskResult JSON。

失败 step 后 finalizer 必须仍然执行：KernelExecutor 捕获该 cell 的 error 后继续发送 finalizer request；不能依赖 notebook runner 自动“失败即停止”。

Result sentinel：

    CRAFTMAKE_TASK_RESULT_BEGIN
    {valid protocol.TaskResult JSON}
    CRAFTMAKE_TASK_RESULT_END

ResultDecoder 只接受匹配 TaskID/RunID/Attempt 的 sentinel，拒绝普通 stdout 中伪造的 JSON。

### 5.6 Jupyter kernel seam

    type KernelExecutor interface {
        Execute(context.Context, KernelTarget, []NotebookCell) ([]CellResult, error)
        Interrupt(context.Context, KernelTarget) error
        Close() error
    }

实现需要处理：WebSocket framing、Jupyter message header/parent_header/metadata/content/buffers、HMAC signing key、execute_request/reply、stream、error、display_data、kernel_info、连接 token 过期与重连。

KernelExecutor 不知道 craftmake TaskResult 结构；NotebookBuilder/ResultDecoder 不知道 WebSocket 细节。两者通过 cell result seam 连接。

### 5.7 日志和 TaskResult

Colab backend 在拿到 TaskResult 后必须：
1. 验证 protocol version、RunID、TaskID、Attempt、status。
2. 通过 Drive API/remote file client 把每个 step stdout/stderr materialize 到现有 `StepManifest.StdoutPath`/`StderrPath`。
3. 把远端路径保存在 metadata 中，把本地路径放入返回给 scheduler 的 StepResult。
4. 原子写入 `manifest.ResultPath`，再返回 TaskOutcome。

这样现有 logs/report 不需要理解 Colab。

## 6. 错误、取消、恢复和缓存

### 6.1 错误分类

- AuthRequired：凭据缺失或 mount 未预授权。
- RuntimeUnavailable：412/503/quota/accelerator unavailable。
- KernelDisconnected：WS 断开、proxy token 过期、runtime death。
- StepFailed：远端 shell exit non-zero。
- OutputMissing：声明输出不存在。
- ProtocolMismatch：TaskResult 不匹配或 sentinel 无效。
- TransferFailed：Drive 上传/下载失败。

### 6.2 Resume

- 已在本地 store 中完成的 task 不重跑，除非 `--force` 或 cache 规则要求。
- 半开 task 没有合法 terminal TaskResult 时视为 pending/failed，不得推断成功。
- runtime death 后创建新 runtime，沿用同一个 Drive run root；新 attempt 使用新 attempt 目录或明确的清理/覆盖策略。建议新 attempt 目录，避免旧状态污染。
- Drive 中的状态文件只能作为诊断和远端幂等辅助；最终任务状态仍以本地 scheduler/store + 合法 TaskResult 为准。
- 不能用“输出文件存在”单独判定 task 成功，因为可能是旧 attempt 的残留。

### 6.3 Cancel

- CancelSubmission：优先 Interrupt 当前 kernel execution，等待 bounded grace period。
- Cancel：取消 keep-alive，unassign runtime，标记本地 run cancelled。
- 外部 `craftmake cancel --run ID` 必须从 store metadata 重新获得 runtime/endpoint/kernel/session 句柄；不能依赖仍存活的 CLI 进程。取消流程在 `CancelSubmission` 后继续执行 run-level cleanup，或调用一个幂等的 `CancelRun`/`FinishRun` adapter。
- runtime 消失时取消操作应幂等；不能因为远端 404 覆盖本地取消状态。

### 6.4 既有 scheduler/recovery 兼容性

- backend registry 统一服务于 run、status、cancel、resume、doctor，禁止在 CLI 各处继续增加 `switch backendName`。
- Colab 实现 `backend.Recoverable`。恢复只认同一个 run/task/attempt 的合法 `result.json`；不能以旧输出文件存在推断成功。
- action 的 `ConfigPath` 可以为空；resume digest 校验改为校验 `WorkflowPath`/action source digest，并按 `LoaderKind` 重新构建 Plan。
- Scheduler 对 Colab 使用 remote readiness/output metadata，或在本地检查前执行已同步的 materialization barrier；不能直接用 host `os.Stat` 判断远端输入/输出。
- host `os.Executable()` 只保留给 local/slurm；Colab backend 必须忽略该参数，不把本机二进制发送到远端。

## 7. 代码布局

```text
internal/engine/
  load.go                 # LoadRequest, LoadedPlan, PlanLoader
  run.go                  # RunEngine / shared runPlan
  backend_factory.go      # backend registry and lifecycle wiring
internal/adapters/action/
  schema.go               # craftmake.action/v1 types
  loader.go               # validate, inputs, env, normalize
  discover.go             # .craftmake/*.yaml
  normalize.go             # action -> WorkflowSpec + Context
internal/backend/
  lifecycle.go            # optional RunLifecycle seam
  registry.go             # local/slurm/colab factory registration
  colab/
    backend.go            # Backend + RunLifecycle
    control_plane.go      # ColabControlPlane
    auth.go               # token storage/refresh/loopback
    drive_auth.go         # mount preflight/auth state
    drive.go              # Drive API + remote files
    workspace.go          # RemoteTaskMapping
    notebook.go           # NotebookBuilder
    kernel.go             # KernelExecutor implementation
    result.go             # sentinel/result decoding
    logs.go               # LogMaterializer
    recovery.go           # Recoverable and Drive-backed attempt recovery
    errors.go             # classified errors
internal/cli/
  action.go               # thin action CLI
  root.go                 # command registration only
pkg/protocol/            # unchanged TaskResult contract initially

tests should be colocated with the public seam they exercise; Colab integration tests use fakes, not real OAuth or a live runtime.
```

## 8. TDD 交付策略（red → green → refactor）

TDD 只测试已确认的 public seam，不测试私有 helper 或内部数据库旁路。每个切片先写一个行为测试，再写最小实现，再重构。

### Slice 0 — 基线保护

- Red: 增加现有 `run`/`resume`/local/slurm 行为的 contract tests，确认重构前后输出、state 和 exit code 不变。
- Green: 抽取共享 engine，不改变现有 CLI 行为。
- Seam：CLI command result + scheduler/backend public interfaces。

### Slice 1 — Action discovery

行为：给定 cwd/.craftmake/a.yaml、b.yml 和子目录文件，list 只返回一级 yaml；非法文件给出 path 定位错误。
- 测试：discover public function/iterator，不读实现内部 map。
- 实现：discover.go。

### Slice 2 — Action schema validation

行为：合法 action/v1 可加载；缺 name/jobs、未知 backend、非法 id、重复 input、缺 required input 都失败；错误包含字段路径。
- 测试：adapter Load seam。
- 实现：schema.go + loader.go。

### Slice 3 — Env/input expression

行为：workflow/job/step env 按层级覆盖；input default/CLI override 正确；未知表达式、非标量表达式、未闭合变量失败；output/env 文件只能传递允许格式。
- 测试：loader/normalizer public seam，使用独立 YAML fixture 和独立 expected values。

### Slice 4 — Normalize and compile

行为：自包含 action 生成一个可被现有 compiler 接受的 Plan；每个 job 变成 global task；needs 保持 DAG；resources/env/steps 保持语义。
- 测试：`PlanLoader.Load` 返回 Plan，断言任务/依赖/step 行为，不断言私有 struct 构造过程。

### Slice 5 — Config-driven action path

行为：`action plan/run NAME --config config.yaml` 与等价 `craftmake run --config config.yaml --workflow .craftmake/NAME.yaml` 产生相同 Plan、digest 和 loader kind；无 config 的旧 WorkflowSpec 给出 actionable error。
- 测试：统一 PlanLoader seam，比较独立序列化的 Plan summary。

### Slice 6 — Shared RunEngine

行为：run 与 action run 使用相同 scheduler/store/envelope；local backend 的 state/report/logs/resume 不回归。
- 测试：RunEngine seam + fake backend；避免直接查询 SQLite 证明结果。

### Slice 7 — Backend lifecycle

行为：实现 RunLifecycle 的 fake backend 在一次 run 中 Begin/End 各一次；失败/取消也 End；旧 backend 不实现时行为不变。
- 测试：Scheduler/RunEngine public run seam。
- 这是 colab “一个 run 一个 runtime”成立的前置 slice。

### Slice 8 — Colab control plane

行为：fake/httptest control plane 能处理 assign、proxy refresh、keep-alive、unassign、412/503/quota/401；token 不写入普通日志或 run metadata。
- 测试：ColabControlPlane interface；真实 endpoint 只做 opt-in smoke test。

### Slice 9 — Drive mount preflight

行为：已授权 mount 继续；未授权在执行前失败并给出授权提示；kernel 不会无限等待 input。
- 测试：DriveMountBootstrap interface + fake kernel。

### Slice 10 — Remote workspace mapping

行为：run/task/attempt 路径稳定；相对路径可解析；path_map 明确允许绝对路径；路径穿越和未映射绝对路径拒绝。
- 测试：RemoteWorkspace interface，使用独立路径样例。

### Slice 11 — Notebook builder

行为：一个 TaskManifest 生成 bootstrap、N 个 step cells、finalizer；cell order/id 稳定；命令/env 正确转义；不把 token 写进 notebook。
- 测试：NotebookBuilder public seam；golden fixture 只验证用户可观察 notebook 结构和内容。

### Slice 12 — Kernel executor

行为：fake Jupyter server 能返回 execute_reply/stream/error/display_data；step error 后 finalizer 仍执行；interrupt 可取消；断线和 token expiry 分类正确。
- 测试：KernelExecutor interface + fake WebSocket server，不测试具体 WS library 私有方法。

### Slice 13 — TaskResult and logs

行为：合法 sentinel 返回 TaskResult；伪造/错 task/错 attempt/缺 sentinel 拒绝；远端日志 materialize 到现有 StepResult paths；manifest.ResultPath 原子写入。
- 测试：ResultDecoder + LogMaterializer seams；fake remote files。

### Slice 14 — Colab backend integration

行为：RunSubmission 对每个 task 产生一个 notebook artifact、按序执行、返回合法 TaskOutcome；一个 run 只创建一个 runtime；workers>1 被拒绝或封顶策略一致。
- 测试：Colab backend interface + fake control plane/kernel/drive；不使用真实账号。

### Slice 15 — Resume/cancel/recovery

行为：runtime death 后新 runtime 复用 Drive root 并创建新 attempt；已完成 task 不重跑；半开 task 不判成功；外部进程执行 cancel 能找到远端 session；cancel 可重复调用；action 的空 ConfigPath 能正确 resume。
- 测试：RunEngine/Scheduler/Colab backend public seams，使用可控 fake failure injection；不得通过直接查询 SQLite 私有表来证明行为。

### Slice 16 — Live smoke test

仅在所有 fake/integration tests 通过后运行 opt-in live test：需要用户凭据、Colab quota 和明确环境变量；不纳入默认 `go test ./...`。

## 9. 验收标准

### Action

- `.craftmake/*.yaml` 可发现；list/plan/run 参数和错误稳定。
- 自包含 action 和 `--config` 既有 WorkflowSpec 两条路径共享 PlanLoader/RunEngine。
- env/input/needs/step output 行为有 contract tests。
- local backend 下完整跑通，并且现有 run/resume/report/logs 无回归。

### Colab

- `craftmake doctor --backend colab` 能区分 control-plane、Drive API、mount authorization、quota 四类状态；若使用 split credentials，要分别报告缺失的 credential set。
- 一个 run 只创建一个 runtime；每个 task 有一个 ipynb artifact；kernel 串行执行 `%%bash` cells。
- Drive root 稳定，输出能在 runtime 结束后保留；新 runtime 可 resume。
- TaskResult、StepResult 和本地日志路径符合现有 protocol。
- 412/503、401、kernel disconnect、Drive transfer、step failure、missing output 都能分类并进入现有 state/report。
- 没有真实凭据时，默认测试完全离线可运行。

## 10. 风险与必须先确认的事项

1. **Colab 控制面兼容性**：参考项目使用的 endpoint/header 可能随 Colab 变化；必须通过 ColabControlPlane 隔离，并接受 live smoke test 可能随时失效。
2. **Drive mount 授权**：这是当前计划中最大的未闭合点。单次 Colab+Drive OAuth 不能自动等价于 runtime mount authorization；实现前必须确定一次性 mount auth 的实际流程和凭据来源。
3. **现有流程绝对路径**：config-driven action 只能在存在 path_map/Drive root 映射时运行；不能承诺透明迁移。
4. **大文件性能**：Drive FUSE 适合持久化但不适合高频临时 IO；v1 优先正确性和可恢复性，后续可增加 ephemeral scratch + checkpoint。
5. **凭据泄露**：notebook、controller log、TaskResult、error message 都必须经过 secret redaction；禁止把 proxy/access token 写入 artifact。
6. **协议变更成本**：尽量不修改 `pkg/protocol.TaskManifest/TaskResult`；Colab-specific metadata 放 backend Raw/本地 sidecar，只有确有必要时才扩展 protocol。

## 11. 实施顺序与提交边界

每个阶段都必须保持可构建、可测试：

1. Baseline contract tests + engine seam extraction。
2. Action discovery/schema/normalization + local execution。
3. Config-driven action path + shared CLI run core。
4. Backend lifecycle seam + registry + colab config validation。
5. Colab control plane/auth/doctor（不执行任务）。
6. Drive mount preflight + workspace mapping。
7. NotebookBuilder + KernelExecutor + ResultDecoder。
8. Colab backend end-to-end fake integration。
9. Resume/cancel/error recovery。
10. Opt-in live smoke test、文档、发布说明。

每一阶段的提交都应遵循：一个行为测试（red）→ 最小实现（green）→ 局部重构；不要先批量写完所有 imagined tests，也不要在未确认 seam 前测试私有实现。

## 12. 最终设计判断

该方案比原计划更统一且更可实施：

- action 是 `PlanLoader` 的输入适配器，不是第二套执行引擎；
- config-driven 与 self-contained action 共享同一 `Plan` 和 `RunEngine`；
- colab 是 Backend + RunLifecycle，不侵入 compiler/scheduler/store 的任务语义；
- notebook、kernel、Drive、control plane 各自拥有窄而深的 seam；
- 远端持久化解决 session timeout，但最终成功状态仍由本地 store + 合法 TaskResult 决定；
- 所有 Colab 不确定性都集中在 `internal/backend/colab`，不会污染 local/slurm 和 action DSL。
- 现有 CLI 的 backend 选择、resume、cancel、doctor 均经过 registry；不存在只在 `run` 支持 colab、而 status/resume 不支持的半集成状态。

该 TDD 可作为实现基线；在开始 Colab live implementation 前，必须先完成 Slice 0–7，并确认第 10 节第 1/2 项：生产 control-plane endpoint 适配版本，以及 Drive mount authorization/credential 流程。若这两项未确认，只实现 fake control-plane、action/local 路径和协议模型，不接入真实账号。