# TDD：Colab 临时实例 + 持久化 Drive 混合加速器调度

## 核心洞察

Google Drive 挂载后就是工作流的**持久化共享存储层**。实例（CPU/GPU）仅仅是**临时算力容器**，用完即毁。

```text
 Job A (cpu)              Job B (gpu)              Job C (cpu)
 ┌────────────┐           ┌────────────┐           ┌────────────┐
 │ Assign CPU │           │ Assign GPU │           │ Assign CPU │
 │ Mount Drive│           │ Mount Drive│           │ Mount Drive│
 │ Execute    │           │ Execute    │           │ Execute    │
 │ Flush Drive│           │ Flush Drive│           │ Flush Drive│
 │ Release    │           │ Release    │           │ Release    │
 └─────┬──────┘           └─────┬──────┘           └────────────┘
       │                        │
       └── Drive: /MyDrive/proj ┘  ← 三个 Job 读写同一份 Drive 数据
```

**与初版 RuntimePool 方案的根本区别**：

| | RuntimePool（初版） | 临时实例 + Drive（本版） |
|---|---|---|
| 实例生命周期 | 跨 Job 复用，Run 结束统一释放 | 每个 Submission 独立分配/释放 |
| 状态传递 | 实例本地文件系统 | Google Drive（持久化） |
| 空闲占用 | GPU 实例在 CPU Job 执行期间空闲挂着 | **零空闲占用**——不用的实例立刻释放 |
| GPU 配额压力 | 高（GPU 实例长期持有） | **最低**——GPU 仅在 GPU Job 执行时占用 |
| 复杂度 | 需要 Pool 并发管理、心跳保活 | **极简**——生命周期封装在 RunSubmission 内部 |
| 并行 CPU+GPU | 需要 Pool 同时持有两个实例 | 自然支持——调度器并发投递两个 Submission |

---

## 设计方案：Submission-Scoped Ephemeral Runtime

### 架构变更

```text
当前架构（单实例）：
  BeginRun:     Assign(一台机器)
  RunSubmission: 复用 b.runtime 执行所有 Task
  EndRun:       Release(那台机器)

新架构（临时实例）：
  BeginRun:     仅加载配置、校验凭据（不分配任何机器）
  RunSubmission: Assign(按 accelerator) → Mount Drive → 执行 → Flush → Release
  EndRun:       确认无残留实例（防御性清理）
```

**关键设计决策**：实例的 Assign/Release 从 `BeginRun/EndRun` **下沉**到 `RunSubmission` 内部，形成自包含的生命周期闭环。

### 核心优势

1. **GPU 配额使用率最优**：GPU 实例仅在 GPU Job 运行的那几秒/几分钟内被占用，前后的 CPU Job 不浪费任何 GPU 时间。
2. **天然兼容 Google 免费层**：免费账户同时只允许 1 个 GPU 实例——因为每个 Job 用完立刻释放，不会冲突。
3. **极简实现**：不需要 RuntimePool、不需要心跳保活、不需要跨 Job 的实例复用逻辑。
4. **容错隔离**：某个 Job 的实例崩溃不会污染其他 Job——下一个 Job 分配全新实例。

---

## YAML Schema

```yaml
schema_version: craftmake.action/v1
name: ml_pipeline
backend: colab
colab:
  session: gpu
  default_accelerator: cpu          # Job 未声明 accelerator 时的默认值
  drive_root: /content/drive/MyDrive/ml_pipeline
jobs:
  preprocess:
    accelerator: cpu                 # → 临时分配 CPU 实例
    steps:
      - run: |
          python3 - << 'EOF'
          import pandas as pd
          # CPU 密集型预处理：数据清洗、特征工程...
          # 结果直接写在 Drive 上
          df = pd.read_csv('/content/drive/MyDrive/ml_pipeline/raw/data.csv')
          df.to_parquet('/content/drive/MyDrive/ml_pipeline/processed/data.parquet')
          EOF

  train:
    accelerator: gpu                 # → 临时分配 GPU 实例
    needs: [preprocess]              # 等 CPU 预处理完成后才启动
    steps:
      - run: |
          python3 - << 'EOF'
          import torch
          # GPU 训练：从 Drive 读取预处理数据
          # 模型检查点写回 Drive
          print('GPU:', torch.cuda.get_device_name(0))
          EOF

  evaluate:
    accelerator: gpu
    needs: [train]
    steps:
      - run: python3 evaluate.py

  report:
    accelerator: cpu                 # → 临时分配 CPU 实例
    needs: [evaluate]
    steps:
      - run: python3 generate_report.py
```

### 并行 CPU + GPU

```yaml
jobs:
  data_qc:
    accelerator: cpu                 # CPU 实例 ①
    steps:
      - run: fastqc data/*.fastq.gz

  gpu_precompute:
    accelerator: gpu                 # GPU 实例 ②（与 data_qc 同时运行）
    steps:
      - run: python3 precompute.py

  integrate:
    accelerator: cpu                 # CPU 实例 ③（等 ①② 都结束才启动）
    needs: [data_qc, gpu_precompute]
    steps:
      - run: python3 integrate.py
```

调度器的 DAG 依赖 + admission 自然保证：
- `data_qc` 和 `gpu_precompute` 无依赖 → 调度器并发投递
- 它们分别申请 CPU 和 GPU 实例 → Google 允许同时 1 CPU + 1 GPU
- `integrate` 等两者都完成才启动

---

## TDD 切片清单

### Phase 1: 数据模型扩展（纯离线）

| # | 测试名 | 红→绿内容 | 涉及文件 |
|---|---|---|---|
| 1.1 | `TestJobSpecAcceleratorField` | `spec.JobSpec` 新增 `Accelerator string` 字段，YAML 反序列化 `accelerator: gpu` | `internal/spec/model.go` |
| 1.2 | `TestJobSpecAcceleratorValidation` | 合法值：`""`, `"cpu"`, `"gpu"`, `"tpu"`；非法值（如 `"quantum"`）返回错误 | `internal/spec/model.go` |
| 1.3 | `TestResourceRequestAccelerator` | `protocol.ResourceRequest` 新增 `Accelerator` 字段，JSON 往返保真 | `pkg/protocol/types.go` |
| 1.4 | `TestTaskCarriesAccelerator` | 编译后 `compiler.Task.Accelerator` = Job 声明值 | `internal/compiler/model.go`, `compiler.go` |
| 1.5 | `TestActionDefaultAccelerator` | `ColabSpec.DefaultAccelerator` 回填到未声明 `accelerator` 的 Job | `internal/adapters/action/loader.go` |
| 1.6 | `TestManifestCarriesAccelerator` | `TaskManifest.Resources.Accelerator` 随 Manifest 下发 | `internal/scheduler/scheduler.go` |

### Phase 2: RunSubmission 自包含生命周期（Mock ControlPlane + Executor）

| # | 测试名 | 红→绿内容 | 涉及文件 |
|---|---|---|---|
| 2.1 | `TestRunSubmissionAssignsPerAccelerator` | RunSubmission 内部按 Manifest.Accelerator 调用 `AcquireRuntime` 传入对应 accelerator | `internal/backend/colab/backend.go` |
| 2.2 | `TestRunSubmissionMountsDrive` | 每次 Assign 后自动触发 Drive 挂载（bootstrap 注入 `drive.mount`） | 同上 + `notebook.go` |
| 2.3 | `TestRunSubmissionReleaseAfterExecution` | 每个 Manifest 执行完毕后立即调用 `ReleaseRuntime` | 同上 |
| 2.4 | `TestRunSubmissionReleaseOnFailure` | 执行失败也保证 Release（defer 语义） | 同上 |
| 2.5 | `TestRunSubmissionDefaultAccelerator` | Manifest 无 Accelerator → 使用 `Config.DefaultAccelerator` (`"cpu"`) | 同上 |
| 2.6 | `TestRunSubmissionCPUThenGPU` | 两个 Manifest 顺序：CPU→GPU → 分别 Assign/Release 两次 | 同上 |
| 2.7 | `TestRunSubmissionConsecutiveSameType` | 两个 CPU Manifest → 分别独立 Assign/Release（不复用，因为 Drive 是持久层） | 同上 |

### Phase 3: BeginRun/EndRun 重构

| # | 测试名 | 红→绿内容 | 涉及文件 |
|---|---|---|---|
| 3.1 | `TestBeginRunNoAssignment` | `BeginRun` 不再调用 `AcquireRuntime`（仅校验配置和凭据） | `backend.go` |
| 3.2 | `TestBeginRunValidatesCredentials` | `BeginRun` 仍然加载 session auth、校验 token 存在性 | 同上 |
| 3.3 | `TestEndRunDefensiveCleanup` | `EndRun` 调用 `ListAssignments` 清理残留实例（防御性） | 同上 |
| 3.4 | `TestEndRunNoopWhenClean` | 无残留实例时 `EndRun` 无副作用 | 同上 |

### Phase 4: RuntimeRequest 扩展

| # | 测试名 | 红→绿内容 | 涉及文件 |
|---|---|---|---|
| 4.1 | `TestRuntimeRequestAccelerator` | `RuntimeRequest` 新增 `Accelerator` 字段 | `backend.go` (接口) |
| 4.2 | `TestServerControlPlanePassesAccelerator` | `ServerControlPlane.AcquireRuntime` 将 Accelerator 映射到 `RuntimeSpec.Accelerator` | `adapter.go` |
| 4.3 | `TestAssignWithGPUAccelerator` | `Assign(RuntimeSpec{Accelerator: "GPU"})` → URL 带 `&accelerator=GPU` | `colab_server.go` |

### Phase 5: 端到端 Action 测试

| # | 测试名 | 红→绿内容 | 涉及文件 |
|---|---|---|---|
| 5.1 | `TestActionPlanShowsAccelerator` | `action plan` 输出显示每个 Task 的加速器标签 | `internal/cli/action.go` |
| 5.2 | `TestMixedPipelineOffline` | 3-Job DAG (cpu→gpu→cpu) 离线编译 → 正确编排 | `internal/cli/action_test.go` |
| 5.3 | `TestParallelAcceleratorOffline` | 2 并行 Job (cpu∥gpu) + 1 合并 Job → 编译顺序正确 | 同上 |

### Phase 6: 实机验证（可选）

| # | 测试名 | 验证内容 |
|---|---|---|
| 6.1 | `TestLiveCPUJobWritesDrive` | CPU 实例执行 → 写 Drive → 释放 → 验证文件在 Drive 上 |
| 6.2 | `TestLiveGPUJobReadsDriveOutput` | GPU 实例执行 → 从 Drive 读取上一步结果 → 写回 Drive |
| 6.3 | `TestLiveMixedPipeline` | 完整 cpu→gpu→cpu 三阶段在真实 Colab 上执行 |

---

## 关键代码变更

### 1. RunSubmission 重构（核心变更）

```go
func (b *Backend) RunSubmission(ctx context.Context, submissionID string, request backend.SubmissionRequest) (*backend.SubmissionResult, error) {
    result := &backend.SubmissionResult{BackendID: "colab:" + submissionID, Tasks: map[string]backend.TaskOutcome{}}

    for _, manifest := range request.Manifests {
        if err := ctx.Err(); err != nil {
            return result, err
        }

        // 1. 确定加速器类型
        accelerator := manifest.Resources.Accelerator
        if accelerator == "" {
            accelerator = b.Config.DefaultAccelerator
        }
        if accelerator == "" {
            accelerator = "cpu"
        }

        // 2. 按需分配临时实例
        runtime, err := b.Control.AcquireRuntime(ctx, RuntimeRequest{
            RunID:       manifest.RunID,
            Accelerator: accelerator,
        })
        if err != nil {
            result.Tasks[manifest.TaskID] = backend.TaskOutcome{Err: err}
            continue
        }

        // 3. 执行（Drive 在 bootstrapSource 中自动挂载）
        outcome := b.executeOnRuntime(ctx, runtime, manifest, request.OnStarted)

        // 4. 立即释放实例——Drive 数据已持久化
        if releaseErr := b.Control.ReleaseRuntime(ctx, runtime); releaseErr != nil {
            // 记录但不覆盖执行结果
            if outcome.Err == nil {
                outcome.Err = fmt.Errorf("release runtime: %w", releaseErr)
            }
        }

        result.Tasks[manifest.TaskID] = outcome
    }
    return result, nil
}
```

### 2. BeginRun 简化

```go
func (b *Backend) BeginRun(ctx context.Context, run backend.RunContext) error {
    // 仅加载配置——不分配任何机器
    config := b.Config
    // ...load session auth, set DriveRoot/MountPath...
    b.Config = config

    // 校验凭据可用（refresh token 能刷到 access token）
    if b.Control != nil {
        if err := b.Control.ValidateCredentials(ctx); err != nil {
            return fmt.Errorf("Colab credentials invalid: %w", err)
        }
    }
    return nil
}
```

### 3. EndRun 防御性清理

```go
func (b *Backend) EndRun(ctx context.Context, outcome backend.RunOutcome) error {
    // 防御性：扫描并释放任何残留实例
    assignments, err := b.Control.ListAssignments(ctx)
    if err != nil {
        return fmt.Errorf("list residual assignments: %w", err)
    }
    for _, a := range assignments {
        _ = b.Control.ReleaseRuntime(ctx, Runtime{ID: a.Endpoint})
    }
    return nil
}
```

### 4. notebook.go bootstrapSource 增强

每个 Job 都是新实例，所以 **每次都需要挂载 Drive**：

```go
func bootstrapSource(mapping RemoteTaskMapping) string {
    // 始终注入 Drive 挂载（因为每次都是新实例）
    mountBlock := `if not os.path.ismount('/content/drive'):
    try:
        from google.colab import drive
        drive.mount('/content/drive', force_remount=False)
    except Exception as _e:
        print(f"drive.mount: {_e}")`

    // ...rest of bootstrap...
}
```

同样，finalizerSource 增加 flush：

```go
func finalizerSource(...) string {
    // 在写入 result.json 之后、退出之前 flush Drive
    flush := `
try:
    from google.colab import drive
    drive.flush_and_unmount()
except:
    pass
`
    // ...
}
```

---

## 实施顺序

```text
Phase 1 (模型)
  │
  ├── 1.1~1.6: spec/protocol/compiler 加 Accelerator 字段
  │
Phase 2 (RunSubmission 重构) ← 核心变更
  │
  ├── 2.1~2.7: RunSubmission 内部 Assign→Execute→Release 闭环
  │
Phase 3 (BeginRun/EndRun 重构)
  │
  ├── 3.1~3.4: BeginRun 不再 Assign、EndRun 防御性清理
  │
Phase 4 (RuntimeRequest 扩展)
  │
  ├── 4.1~4.3: Accelerator 参数从 Manifest 传到 Colab API
  │
Phase 5 (端到端 Action)
  │
  ├── 5.1~5.3: action plan/run 显示和路由加速器标签
  │
Phase 6 (实机验证)
  │
  └── 6.1~6.3: 真实 CPU→GPU→CPU 三阶段 Drive 数据流
```

每个 Phase 通过 `go test ./...` 后独立提交，不合并到 main。
