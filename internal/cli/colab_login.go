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

// Bundled Colab OAuth client.
//
// These are the public credentials of Google's Cloud SDK "installed app"
// client. It is the client the official Colab CLI bundles in
// src/colab_cli/oauth_config.json (project "colab-cli", Apache-2.0) and the one
// colab-vscode refers to with the environment variable
// COLAB_EXTENSION_CLIENT_NOT_SO_SECRET. Installed-app client secrets are not
// confidential by design (a desktop app cannot keep one), which is why Google
// ships it inside its own tooling.
//
// Bundling it means `craftmake colab auth login` and `colab drive mount
// --authorize` work with no setup. Override it in any of these ways when you
// want your own OAuth client (recommended for distributions):
//
//	CRAFTMAKE_COLAB_CLIENT_ID / CRAFTMAKE_COLAB_CLIENT_SECRET   (runtime)
//	--client-id / --client-secret                               (login flags)
//	go build -ldflags "-X github.com/otterlab-bio/craftmake/internal/cli.defaultColabClientID=<id> \
//	                   -X github.com/otterlab-bio/craftmake/internal/cli.defaultColabClientSecret=<secret>"
//
// `make build COLAB_CLIENT_ID=... COLAB_CLIENT_SECRET=...` wires the last form.
var (
	// NOTE: the client secret published on the repository's main branch is one
	// character short of the real value and fails with "invalid_client"; the
	// released wheel (google-colab-cli 0.7.4, colab_cli/oauth_config.json)
	// carries the working 24-character secret used here.
	defaultColabClientID     = "764086051850-6qr4p6gpi6hn506pt8ejuq83di341hur.apps.googleusercontent.com"
	defaultColabClientSecret = "d-FL95Q19q7MQmFpd7hHD0Ty"
)

// resolveColabOAuthCredentials resolves the OAuth client for the loopback login
// and for token refresh. Precedence: explicit flag, environment, bundled client.
func resolveColabOAuthCredentials(clientID, clientSecret string) (string, string, error) {
	if clientID == "" {
		clientID = os.Getenv("CRAFTMAKE_COLAB_CLIENT_ID")
	}
	if clientSecret == "" {
		clientSecret = os.Getenv("CRAFTMAKE_COLAB_CLIENT_SECRET")
	}
	if clientID == "" {
		clientID = defaultColabClientID
	}
	if clientSecret == "" {
		clientSecret = defaultColabClientSecret
	}
	if clientID == "" || clientSecret == "" {
		return "", "", fmt.Errorf("Colab OAuth credentials are required; set CRAFTMAKE_COLAB_CLIENT_ID and CRAFTMAKE_COLAB_CLIENT_SECRET, pass --client-id and --client-secret, or build with a bundled client")
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
		auth := colab.SessionAuth{SessionID: sessionID, DriveRoot: driveRoot, MountPath: mountPath, ColabCredentialFile: credentialFile, DriveCredentialFile: credentialFile, OAuthClientID: clientID}
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
