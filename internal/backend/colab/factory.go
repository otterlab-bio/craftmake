package colab

import (
	"context"
	"fmt"

	backendpkg "github.com/otterlab-bio/craftmake/internal/backend"
)

type FactoryDependencies struct {
	Control        ControlPlane
	Executor       NotebookExecutor
	MountPreflight DriveMountPreflight
	Materializer   LogMaterializer
	Workspace      WorkspaceSyncer
	ResultReader   RemoteResultReader
	// Server wires the reference-informed control-plane and proxy execution
	// into the backend when explicit Control/Executor are not supplied.
	Server      *ColabServerClient
	RuntimeSpec RuntimeSpec
	ProxyToken  string
}

func NewFactory(dependencies FactoryDependencies) backendpkg.Factory {
	return func(ctx context.Context, options backendpkg.FactoryConfig) (backendpkg.Backend, error) {
		config := Config{LocalRoot: options.ProjectDirectory, AuthConfigPath: options.AuthConfigPath, SessionID: options.SessionID}
		if config.AuthConfigPath != "" {
			if config.SessionID == "" {
				return nil, fmt.Errorf("Colab session id is required when auth config is set")
			}
			authConfig, err := ConfigForSession(config.AuthConfigPath, config.SessionID)
			if err != nil {
				return nil, err
			}
			config.DriveRoot, config.MountPath = authConfig.DriveRoot, authConfig.MountPath
		}
		control := dependencies.Control
		executor := dependencies.Executor
		if dependencies.Server != nil {
			if control == nil {
				control = &ServerControlPlane{Client: dependencies.Server, Spec: dependencies.RuntimeSpec}
			}
			if executor == nil {
				executor = &JupyterWebSocketExecutor{SessionID: config.SessionID, ColabClient: dependencies.Server}
			}
		}
		if control == nil || executor == nil {
			return nil, fmt.Errorf("Colab factory requires control plane and notebook executor")
		}
		_ = ctx
		return &Backend{Config: config, Control: control, Executor: executor, MountPreflight: dependencies.MountPreflight, Materializer: dependencies.Materializer, Workspace: dependencies.Workspace, ResultReader: dependencies.ResultReader}, nil
	}
}
