package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	colab "github.com/otterlab-bio/craftmake/internal/backend/colab"
	"github.com/spf13/cobra"
)

// Colab OAuth credentials are intentionally supplied at runtime rather than
// embedded in the binary or repository. Set CRAFTMAKE_COLAB_CLIENT_ID and
// CRAFTMAKE_COLAB_CLIENT_SECRET, or pass --client-id/--client-secret to login.
const (
	defaultColabClientID     = ""
	defaultColabClientSecret = ""
)

func resolveColabOAuthCredentials(clientID, clientSecret string) (string, string, error) {
	if clientID == "" {
		clientID = os.Getenv("CRAFTMAKE_COLAB_CLIENT_ID")
	}
	if clientSecret == "" {
		clientSecret = os.Getenv("CRAFTMAKE_COLAB_CLIENT_SECRET")
	}
	if clientID == "" || clientSecret == "" {
		return "", "", fmt.Errorf("Colab OAuth credentials are required; set CRAFTMAKE_COLAB_CLIENT_ID and CRAFTMAKE_COLAB_CLIENT_SECRET or pass --client-id and --client-secret")
	}
	return clientID, clientSecret, nil
}

// colabRequiredScopes matches colab-vscode / colab-cli: Colab uses the
// colaboratory scope, not the Drive scope.
var colabRequiredScopes = []string{"profile", "email", "https://www.googleapis.com/auth/colaboratory"}

func newColabAuthLoginCommand() *cobra.Command {
	var configPath, sessionID, driveRoot, mountPath, clientID, clientSecret, credentialFile string
	var timeout time.Duration
	command := &cobra.Command{Use: "login", Short: "Authenticate a Colab session via loopback OAuth", Args: noArguments, RunE: func(command *cobra.Command, _ []string) error {
		if sessionID == "" {
			return usageError("--session is required")
		}
		var credentialErr error
		clientID, clientSecret, credentialErr = resolveColabOAuthCredentials(clientID, clientSecret)
		if credentialErr != nil {
			return configurationError(credentialErr)
		}
		path, err := expandUserPath(configPath)
		if err != nil {
			return usageError("invalid --config: %v", err)
		}
		if credentialFile == "" {
			credentialFile = filepath.Join(filepath.Dir(path), "credentials", sessionID+".json")
		}
		if driveRoot == "" {
			driveRoot = "/content/drive/MyDrive/craftmake"
		}
		if mountPath == "" {
			mountPath = "/content/drive"
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

		oauth := colab.OAuthConfig{ClientID: clientID, ClientSecret: clientSecret, Scopes: colabRequiredScopes, AuthURL: os.Getenv("CRAFTMAKE_COLAB_AUTH_URL"), TokenURL: os.Getenv("CRAFTMAKE_COLAB_TOKEN_URL"), UserInfoURL: os.Getenv("CRAFTMAKE_COLAB_USERINFO_URL")}
		authURL := oauth.AuthorizationURL(state, redirectURI, challenge)
		fmt.Fprintf(command.OutOrStdout(), "\nOpen this URL in your browser to authorize:\n%s\n\nWaiting for authorization...\n", authURL)
		_ = openBrowser(authURL)

		result, err := server.Wait(command.Context(), timeout)
		if err != nil {
			return backendFailureError(fmt.Errorf("authentication failed: %w", err))
		}
		if result.Err != nil {
			return backendFailureError(fmt.Errorf("authentication failed: %w", result.Err))
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
		auth := colab.SessionAuth{SessionID: sessionID, DriveRoot: driveRoot, MountPath: mountPath, ColabCredentialFile: credentialFile, DriveCredentialFile: credentialFile}
		if err := colab.UpsertSessionAuth(path, auth); err != nil {
			return configurationError(err)
		}
		fmt.Fprintf(command.OutOrStdout(), "\nAuthenticated as %s <%s>\nsession: %s\ncredential: %s\nconfig: %s\n", user.Name, user.Email, sessionID, credentialFile, path)
		return nil
	}}
	command.Flags().StringVar(&configPath, "config", "~/.config/craftmake/colab-auth.json", "Authentication config path")
	command.Flags().StringVar(&sessionID, "session", "", "Named Colab session")
	command.Flags().StringVar(&driveRoot, "drive-root", "/content/drive/MyDrive/craftmake", "Mounted Drive directory used as durable workspace")
	command.Flags().StringVar(&mountPath, "mount-path", "/content/drive", "Runtime Drive mount point")
	command.Flags().StringVar(&clientID, "client-id", "", "OAuth client id (or CRAFTMAKE_COLAB_CLIENT_ID)")
	command.Flags().StringVar(&clientSecret, "client-secret", "", "OAuth client secret (or CRAFTMAKE_COLAB_CLIENT_SECRET)")
	command.Flags().StringVar(&credentialFile, "credential-file", "", "Credential file to write the refresh token to")
	command.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "How long to wait for authorization")
	return command
}

// writeCredentialFile writes a refresh token to a 0600 file, creating parent
// directories with 0700.
func writeCredentialFile(path, refreshToken string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(refreshToken+"\n"), 0o600)
}

// BrowserOpener allows mocking or intercepting browser launch in unit tests.
var BrowserOpener func(url string) error

// openBrowser best-effort opens a URL in the default browser.
func openBrowser(url string) error {
	if BrowserOpener != nil {
		return BrowserOpener(url)
	}
	// Never launch an external browser process inside automated test runners or headless mode.
	if os.Getenv("CRAFTMAKE_NO_BROWSER") == "1" || isRunningInTest() {
		return nil
	}
	for _, cmd := range [][]string{{"open", url}, {"xdg-open", url}, {"cmd", "/c", "start", url}} {
		if err := exec.Command(cmd[0], cmd[1:]...).Start(); err == nil {
			return nil
		}
	}
	return fmt.Errorf("could not open browser automatically")
}

func isRunningInTest() bool {
	return strings.HasSuffix(os.Args[0], ".test") || flag.Lookup("test.v") != nil
}
