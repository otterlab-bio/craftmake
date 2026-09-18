package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	colab "github.com/otterlab-bio/craftmake/internal/backend/colab"
	"github.com/spf13/cobra"
)

func newColabCommand() *cobra.Command {
	parent := &cobra.Command{Use: "colab", Short: "Configure and inspect Google Colab execution"}
	auth := &cobra.Command{Use: "auth", Short: "Manage Colab session authentication"}
	auth.AddCommand(newColabAuthLoginCommand(), newColabAuthConfigureCommand(), newColabAuthShowCommand())
	drive := &cobra.Command{Use: "drive", Short: "Manage Google Drive session mounts"}
	drive.AddCommand(newColabDriveMountCommand())
	parent.AddCommand(auth, drive, newColabDoctorCommand())
	return parent
}

func newColabAuthConfigureCommand() *cobra.Command {
	var configPath, sessionID, driveRoot, mountPath, colabCredential, driveCredential, colabTokenEnv, driveTokenEnv string
	command := &cobra.Command{Use: "configure", Short: "Write credentials references for a Colab session", Args: noArguments, RunE: func(command *cobra.Command, _ []string) error {
		path, err := expandUserPath(configPath)
		if err != nil {
			return usageError("invalid --config: %v", err)
		}
		auth := colab.SessionAuth{SessionID: sessionID, DriveRoot: driveRoot, MountPath: mountPath, ColabCredentialFile: colabCredential, DriveCredentialFile: driveCredential, ColabRefreshTokenEnv: colabTokenEnv, DriveRefreshTokenEnv: driveTokenEnv}
		if err := colab.UpsertSessionAuth(path, auth); err != nil {
			return configurationError(err)
		}
		fmt.Fprintf(command.OutOrStdout(), "session: %s\nconfig: %s\ndrive_root: %s\nmount_path: %s\n", sessionID, path, driveRoot, mountPath)
		return nil
	}}
	command.Flags().StringVar(&configPath, "config", "~/.config/craftmake/colab-auth.json", "Authentication config path")
	command.Flags().StringVar(&sessionID, "session", "", "Named Colab session")
	command.Flags().StringVar(&driveRoot, "drive-root", "", "Mounted Drive directory used as durable workspace")
	command.Flags().StringVar(&mountPath, "mount-path", "/content/drive", "Runtime Drive mount point")
	command.Flags().StringVar(&colabCredential, "colab-credential-file", "", "Colab credential file reference")
	command.Flags().StringVar(&driveCredential, "drive-credential-file", "", "Drive credential file reference")
	command.Flags().StringVar(&colabTokenEnv, "colab-refresh-token-env", "CRAFTMAKE_COLAB_REFRESH_TOKEN", "Environment variable containing Colab refresh token")
	command.Flags().StringVar(&driveTokenEnv, "drive-refresh-token-env", "CRAFTMAKE_DRIVE_REFRESH_TOKEN", "Environment variable containing Drive refresh token")
	return command
}

func newColabAuthShowCommand() *cobra.Command {
	var configPath, sessionID string
	command := &cobra.Command{Use: "show", Short: "Show non-secret session authentication configuration", Args: noArguments, RunE: func(command *cobra.Command, _ []string) error {
		path, err := expandUserPath(configPath)
		if err != nil {
			return usageError("invalid --config: %v", err)
		}
		auth, err := colab.LoadSessionAuth(path, sessionID)
		if err != nil {
			return configurationError(err)
		}
		data, _ := json.MarshalIndent(auth, "", "  ")
		fmt.Fprintln(command.OutOrStdout(), string(data))
		return nil
	}}
	command.Flags().StringVar(&configPath, "config", "~/.config/craftmake/colab-auth.json", "Authentication config path")
	command.Flags().StringVar(&sessionID, "session", "", "Named Colab session")
	return command
}

func newColabDriveMountCommand() *cobra.Command {
	var configPath, sessionID string
	var authorize bool
	var timeout time.Duration
	command := &cobra.Command{Use: "mount", Short: "Authorize and configure Drive mount for a Colab session", Args: noArguments, RunE: func(command *cobra.Command, _ []string) error {
		if sessionID == "" {
			return usageError("--session is required")
		}
		path, err := expandUserPath(configPath)
		if err != nil {
			return usageError("invalid --config: %v", err)
		}
		auth, err := colab.LoadSessionAuth(path, sessionID)
		if err != nil {
			return configurationError(err)
		}
		if !authorize {
			plan := map[string]any{"session": auth.SessionID, "auth_config": path, "mount_path": auth.MountPath, "drive_root": auth.DriveRoot, "status": "ready-for-backend-mount"}
			data, _ := json.MarshalIndent(plan, "", "  ")
			fmt.Fprintln(command.OutOrStdout(), string(data))
			return nil
		}

		client := colab.NewColabServerClient(os.Getenv("CRAFTMAKE_COLAB_DOMAIN"), os.Getenv("CRAFTMAKE_COLAB_GAPI_DOMAIN"), nil)
		client.AppName = "craftmake"
		client.ExtensionVersion = "0.1.0"
		if refreshToken, ok := resolveColabRefreshToken(auth); ok {
			clientID, clientSecret, credentialErr := resolveColabOAuthCredentials("", "")
			if credentialErr != nil {
				return configurationError(credentialErr)
			}
			manager := &colab.TokenManager{Config: colab.TokenConfig{ClientID: clientID, ClientSecret: clientSecret, TokenURL: os.Getenv("CRAFTMAKE_COLAB_TOKEN_URL")}}
			manager.SetRefreshToken(refreshToken)
			client.GetAccessToken = func() (string, error) { return manager.AccessToken(context.Background()) }
		} else {
			return configurationError(fmt.Errorf("session %q has no valid refresh token configured; run 'craftmake colab auth login --session %s' first", sessionID, sessionID))
		}

		ctx := command.Context()
		fmt.Fprintf(command.OutOrStdout(), "Checking Google Drive authorization for session %q...\n", sessionID)
		spec := colab.RuntimeSpec{NotebookHash: colab.NotebookHash("drive-mount-" + sessionID)}
		assignment, err := client.Assign(ctx, spec)
		if err != nil {
			return backendFailureError(fmt.Errorf("acquire probe runtime: %w", err))
		}
		defer func() {
			_ = client.Unassign(context.Background(), assignment.Endpoint)
		}()

		probe, err := client.PropagateCredentials(ctx, assignment.Endpoint, "dfs_ephemeral", true)
		if err != nil {
			return backendFailureError(fmt.Errorf("check drive credentials: %w", err))
		}

		if probe.Success {
			_, _ = client.PropagateCredentials(ctx, assignment.Endpoint, "dfs_ephemeral", false)
			fmt.Fprintf(command.OutOrStdout(), "Google Drive is already authorized for session %q.\nRuntimes will automatically mount Drive at %s.\n", sessionID, auth.MountPath)
			return nil
		}

		if probe.UnauthorizedRedirectURI == "" {
			return backendFailureError(fmt.Errorf("drive authorization returned no redirect URL"))
		}

		fmt.Fprintf(command.OutOrStdout(), "\nGoogle Drive authorization required for session %q.\nOpen this URL in your browser to grant Drive access to Colab:\n%s\n\nWaiting for authorization (press Enter once authorized in browser, or wait for auto-detection)...\n", sessionID, probe.UnauthorizedRedirectURI)
		_ = openBrowser(probe.UnauthorizedRedirectURI)

		deadline := time.Now().Add(timeout)
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		enterCh := make(chan struct{}, 1)
		go func() {
			var buf [1]byte
			_, _ = os.Stdin.Read(buf[:])
			enterCh <- struct{}{}
		}()

		checkAndComplete := func() bool {
			res, err := client.PropagateCredentials(ctx, assignment.Endpoint, "dfs_ephemeral", false)
			if err == nil && res.Success {
				fmt.Fprintf(command.OutOrStdout(), "\nGoogle Drive successfully authorized for session %q!\nFuture runs will automatically mount Google Drive at %s.\n", sessionID, auth.MountPath)
				return true
			}
			return false
		}

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-enterCh:
				if checkAndComplete() {
					return nil
				}
				fmt.Fprintf(command.OutOrStdout(), "Still waiting for browser confirmation... Press Enter again after allowing in browser.\n")
			case <-ticker.C:
				if time.Now().After(deadline) {
					return backendFailureError(fmt.Errorf("Google Drive authorization timed out after %v", timeout))
				}
				if checkAndComplete() {
					return nil
				}
			}
		}
	}}
	command.Flags().StringVar(&configPath, "config", "~/.config/craftmake/colab-auth.json", "Authentication config path")
	command.Flags().StringVar(&sessionID, "session", "", "Named Colab session")
	command.Flags().BoolVar(&authorize, "authorize", false, "Authorize Google Drive access for this session on Google Colab")
	command.Flags().DurationVar(&timeout, "timeout", 3*time.Minute, "How long to wait for authorization")
	return command
}

func newColabDoctorCommand() *cobra.Command {
	var configPath, sessionID string
	command := &cobra.Command{Use: "doctor", Short: "Check offline Colab session readiness", Args: noArguments, RunE: func(command *cobra.Command, _ []string) error {
		path, err := expandUserPath(configPath)
		if err != nil {
			return usageError("invalid --config: %v", err)
		}
		checks := []map[string]any{}
		if _, statErr := os.Stat(path); statErr != nil {
			checks = append(checks, map[string]any{"name": "auth_config", "ok": false, "detail": statErr.Error()})
		} else {
			checks = append(checks, map[string]any{"name": "auth_config", "ok": true, "detail": path})
		}
		if sessionID == "" {
			checks = append(checks, map[string]any{"name": "session", "ok": false, "detail": "--session is required"})
		} else if auth, loadErr := colab.LoadSessionAuth(path, sessionID); loadErr != nil {
			checks = append(checks, map[string]any{"name": "session", "ok": false, "detail": loadErr.Error()})
		} else {
			checks = append(checks, map[string]any{"name": "session", "ok": true, "detail": auth.SessionID})
			checks = append(checks, map[string]any{"name": "control_plane_credential", "ok": auth.ColabCredentialFile != "" || auth.ColabRefreshTokenEnv != ""})
			checks = append(checks, map[string]any{"name": "drive_credential", "ok": auth.DriveCredentialFile != "" || auth.DriveRefreshTokenEnv != ""})
			checks = append(checks, map[string]any{"name": "mount_config", "ok": auth.MountPath != "" && auth.DriveRoot != ""})
		}
		ready := true
		for _, check := range checks {
			if ok, _ := check["ok"].(bool); !ok {
				ready = false
			}
		}
		data, _ := json.MarshalIndent(map[string]any{"session": sessionID, "auth_config": path, "ready": ready, "mode": "offline-preflight", "checks": checks}, "", "  ")
		fmt.Fprintln(command.OutOrStdout(), string(data))
		if !ready {
			return configurationError(fmt.Errorf("Colab session is not ready"))
		}
		return nil
	}}
	command.Flags().StringVar(&configPath, "config", "~/.config/craftmake/colab-auth.json", "Authentication config path")
	command.Flags().StringVar(&sessionID, "session", "", "Named Colab session")
	return command
}

func expandUserPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is empty")
	}
	if path != "~" && len(path) > 2 && path[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path[2:])
	}
	return filepath.Abs(path)
}
