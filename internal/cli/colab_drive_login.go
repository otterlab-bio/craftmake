package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	colab "github.com/otterlab-bio/craftmake/internal/backend/colab"
	"github.com/spf13/cobra"
)

// Bundled Drive OAuth client.
//
// Mounting Drive needs a client with Drive API scopes, which the Colab runtime
// client deliberately does not have. These are rclone's public credentials: the
// same "installed app" client that rclone, colab-cli and other tools ship, and
// installed-app client secrets are not confidential by design. Override them
// with CRAFTMAKE_COLAB_DRIVE_CLIENT_ID / CRAFTMAKE_COLAB_DRIVE_CLIENT_SECRET,
// with --client-id/--client-secret, or at build time via
// `make build DRIVE_CLIENT_ID=... DRIVE_CLIENT_SECRET=...`.
var (
	defaultDriveClientID     = "202264815644.apps.googleusercontent.com"
	defaultDriveClientSecret = "X4Z3ca8xfWDb1Voo-F9a7ZxJ"
)

// resolveDriveOAuthCredentials resolves the Drive-scoped OAuth client.
// Precedence: explicit flag, environment, bundled client.
func resolveDriveOAuthCredentials(clientID, clientSecret string) (string, string, error) {
	if clientID == "" {
		clientID = os.Getenv("CRAFTMAKE_COLAB_DRIVE_CLIENT_ID")
	}
	if clientSecret == "" {
		clientSecret = os.Getenv("CRAFTMAKE_COLAB_DRIVE_CLIENT_SECRET")
	}
	if clientID == "" {
		clientID = defaultDriveClientID
	}
	if clientSecret == "" {
		clientSecret = defaultDriveClientSecret
	}
	if clientID == "" || clientSecret == "" {
		return "", "", fmt.Errorf("Drive OAuth credentials are required; set CRAFTMAKE_COLAB_DRIVE_CLIENT_ID and CRAFTMAKE_COLAB_DRIVE_CLIENT_SECRET or pass --client-id and --client-secret")
	}
	return clientID, clientSecret, nil
}

// resolveDriveRefreshToken returns the Drive-scoped refresh token recorded for
// the session, mirroring resolveColabRefreshToken: the configured credential
// file first, then ~/.config/craftmake/credentials/<session>-drive.json, then
// the configured or global environment variable.
func resolveDriveRefreshToken(auth colab.SessionAuth) (string, bool) {
	if auth.DriveCredentialFile != "" {
		if path, err := expandUserPath(auth.DriveCredentialFile); err == nil {
			if data, err := os.ReadFile(path); err == nil {
				if token := strings.TrimSpace(string(data)); token != "" {
					return token, true
				}
			}
		}
	}
	if auth.SessionID != "" {
		credPath := filepath.Join("~", ".config", "craftmake", "credentials", auth.SessionID+"-drive.json")
		if defaultCredPath, err := expandUserPath(credPath); err == nil {
			if data, readErr := os.ReadFile(defaultCredPath); readErr == nil {
				if token := strings.TrimSpace(string(data)); token != "" {
					return token, true
				}
			}
		}
	}
	if auth.DriveRefreshTokenEnv != "" {
		if token := os.Getenv(auth.DriveRefreshTokenEnv); strings.TrimSpace(token) != "" {
			return strings.TrimSpace(token), true
		}
	}
	if token := os.Getenv("CRAFTMAKE_DRIVE_REFRESH_TOKEN"); strings.TrimSpace(token) != "" {
		return strings.TrimSpace(token), true
	}
	return "", false
}

func newColabDriveLoginCommand() *cobra.Command {
	var configPath, sessionID, clientID, clientSecret, credentialFile string
	var timeout time.Duration
	command := &cobra.Command{Use: "login", Short: "Authorize the Drive-scoped credential used to mount Google Drive", Args: noArguments, RunE: func(command *cobra.Command, _ []string) error {
		if sessionID == "" {
			return usageError("--session is required")
		}
		clientID, clientSecret, credentialErr := resolveDriveOAuthCredentials(clientID, clientSecret)
		if credentialErr != nil {
			return configurationError(credentialErr)
		}
		path, err := expandUserPath(configPath)
		if err != nil {
			return usageError("invalid --config: %v", err)
		}
		if credentialFile == "" {
			credentialFile = filepath.Join(filepath.Dir(path), "credentials", sessionID+"-drive.json")
		}

		verifier, challenge, err := colab.GeneratePKCE()
		if err != nil {
			return internalFailureError(err)
		}
		state := "nonce=" + sessionID
		server, redirectURI, err := colab.StartLoopbackServer(state)
		if err != nil {
			return internalFailureError(err)
		}
		defer server.Close()

		oauth := colab.OAuthConfig{ClientID: clientID, ClientSecret: clientSecret, Scopes: colab.DriveOAuthScopes, AuthURL: os.Getenv("CRAFTMAKE_COLAB_AUTH_URL"), TokenURL: os.Getenv("CRAFTMAKE_COLAB_TOKEN_URL"), UserInfoURL: os.Getenv("CRAFTMAKE_COLAB_USERINFO_URL")}
		authURL := oauth.AuthorizationURL(state, redirectURI, challenge)
		fmt.Fprintf(command.OutOrStdout(), "\nOpen this URL in your browser to authorize Drive access:\n%s\n\nWaiting for authorization...\n", authURL)
		_ = openBrowser(authURL)

		result, err := server.Wait(command.Context(), timeout)
		if err != nil {
			return backendFailureError(fmt.Errorf("Drive authorization failed: %w", err))
		}
		if result.Err != nil {
			return backendFailureError(fmt.Errorf("Drive authorization failed: %w", result.Err))
		}
		token, err := colab.ExchangeCode(command.Context(), colab.TokenConfig{ClientID: clientID, ClientSecret: clientSecret, TokenURL: os.Getenv("CRAFTMAKE_COLAB_TOKEN_URL")}, result.Code, verifier, redirectURI)
		if err != nil {
			return backendFailureError(err)
		}
		if token.RefreshToken == "" {
			return backendFailureError(fmt.Errorf("OAuth response did not include a refresh token"))
		}
		user, err := colab.FetchGoogleUser(command.Context(), oauth, token.AccessToken)
		if err != nil {
			return backendFailureError(err)
		}
		if err := writeCredentialFile(credentialFile, token.RefreshToken); err != nil {
			return internalFailureError(err)
		}
		// Merge into the existing session so the Colab credential is preserved.
		auth, err := colab.LoadSessionAuth(path, sessionID)
		if err != nil {
			return configurationError(fmt.Errorf("load session %q before recording Drive credentials: %w", sessionID, err))
		}
		auth.DriveCredentialFile = credentialFile
		auth.DriveOAuthClientID = clientID
		auth.DriveAccountEmail = user.Email
		if err := colab.UpsertSessionAuth(path, auth); err != nil {
			return configurationError(err)
		}
		fmt.Fprintf(command.OutOrStdout(), "\nDrive authorized as %s <%s>\nsession: %s\ncredential: %s\nclient: %s\n", user.Name, user.Email, sessionID, credentialFile, clientID)
		return nil
	}}
	command.Flags().StringVar(&configPath, "config", "~/.config/craftmake/colab-auth.json", "Authentication config path")
	command.Flags().StringVar(&sessionID, "session", "", "Named Colab session")
	command.Flags().StringVar(&clientID, "client-id", "", "Drive OAuth client id (or CRAFTMAKE_COLAB_DRIVE_CLIENT_ID)")
	command.Flags().StringVar(&clientSecret, "client-secret", "", "Drive OAuth client secret (or CRAFTMAKE_COLAB_DRIVE_CLIENT_SECRET)")
	command.Flags().StringVar(&credentialFile, "credential-file", "", "File to write the Drive refresh token to")
	command.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "How long to wait for authorization")
	return command
}

func newColabDriveLogoutCommand() *cobra.Command {
	var configPath, sessionID string
	command := &cobra.Command{Use: "logout", Short: "Forget the stored Drive credential for a session", Args: noArguments, RunE: func(command *cobra.Command, _ []string) error {
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
		if auth.DriveCredentialFile != "" {
			if credentialPath, expandErr := expandUserPath(auth.DriveCredentialFile); expandErr == nil {
				if removeErr := os.Remove(credentialPath); removeErr != nil && !os.IsNotExist(removeErr) {
					return internalFailureError(removeErr)
				}
			}
		}
		auth.DriveCredentialFile = ""
		auth.DriveOAuthClientID = ""
		auth.DriveAccountEmail = ""
		auth.DriveRefreshTokenEnv = "CRAFTMAKE_DRIVE_REFRESH_TOKEN"
		if err := colab.UpsertSessionAuth(path, auth); err != nil {
			return configurationError(err)
		}
		fmt.Fprintf(command.OutOrStdout(), "Drive credentials removed for session %s\n", sessionID)
		return nil
	}}
	command.Flags().StringVar(&configPath, "config", "~/.config/craftmake/colab-auth.json", "Authentication config path")
	command.Flags().StringVar(&sessionID, "session", "", "Named Colab session")
	return command
}

// driveAccessTokenManager builds the token manager for the session's Drive
// credential and returns the refresh token backing it. Both are empty when the
// session has no Drive credential.
func driveAccessTokenManager(auth colab.SessionAuth) (*colab.TokenManager, string, error) {
	refreshToken, ok := resolveDriveRefreshToken(auth)
	if !ok {
		return nil, "", nil
	}
	clientID, clientSecret, err := resolveDriveOAuthCredentials("", "")
	if err != nil {
		return nil, "", err
	}
	if auth.DriveOAuthClientID != "" && auth.DriveOAuthClientID != clientID {
		return nil, "", fmt.Errorf("session %q Drive access was authorized with OAuth client %s, but the configured client is %s; refresh tokens cannot be moved between clients, so run `craftmake colab drive login --session %s` again", auth.SessionID, auth.DriveOAuthClientID, clientID, auth.SessionID)
	}
	manager := &colab.TokenManager{Config: colab.TokenConfig{ClientID: clientID, ClientSecret: clientSecret, TokenURL: os.Getenv("CRAFTMAKE_COLAB_TOKEN_URL")}}
	manager.SetRefreshToken(refreshToken)
	return manager, refreshToken, nil
}

// driveResumeDir is where resumable upload sessions are remembered: a large
// upload that a crash or a kill interrupted continues on the next run instead of
// starting over. It lives in craftmake's own state directory, which is excluded
// from the workspace sync and from git.
func driveResumeDir(projectDir string) string {
	if override := strings.TrimSpace(os.Getenv("CRAFTMAKE_COLAB_DRIVE_RESUME_DIR")); override != "" {
		return override
	}
	if strings.TrimSpace(projectDir) == "" {
		return ""
	}
	return filepath.Join(projectDir, ".craftmake", "state", "drive-uploads")
}

// newColabDriveStore builds the Drive REST store for a session, or returns nil
// when the session has no Drive credential.
func newColabDriveStore(auth colab.SessionAuth, projectDir string) (colab.DriveStore, error) {
	manager, _, err := driveAccessTokenManager(auth)
	if err != nil {
		return nil, err
	}
	if manager == nil {
		return nil, fmt.Errorf("colab.drive_transport=rest requires a Drive credential; run `craftmake colab drive login --session %s` first", auth.SessionID)
	}
	store := colab.NewDriveRESTClient(func() (string, error) { return manager.AccessToken(context.Background()) }, nil)
	store.SessionDir = driveResumeDir(projectDir)
	return store, nil
}
