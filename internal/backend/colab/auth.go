package colab

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const AuthSchemaVersion = "craftmake.colab-auth/v1"

type AuthFile struct {
	SchemaVersion string                 `json:"schema_version"`
	Sessions      map[string]SessionAuth `json:"sessions"`
}

type SessionAuth struct {
	SessionID            string `json:"session_id"`
	ColabCredentialFile  string `json:"colab_credential_file,omitempty"`
	DriveCredentialFile  string `json:"drive_credential_file,omitempty"`
	ColabRefreshTokenEnv string `json:"colab_refresh_token_env,omitempty"`
	DriveRefreshTokenEnv string `json:"drive_refresh_token_env,omitempty"`
	DriveRoot            string `json:"drive_root"`
	MountPath            string `json:"mount_path"`
}

func LoadAuthFile(path string) (AuthFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AuthFile{}, fmt.Errorf("read Colab auth config: %w", err)
	}
	var file AuthFile
	if err := json.Unmarshal(data, &file); err != nil {
		return AuthFile{}, fmt.Errorf("parse Colab auth config: %w", err)
	}
	if file.SchemaVersion != AuthSchemaVersion {
		return AuthFile{}, fmt.Errorf("auth schema_version must be %q", AuthSchemaVersion)
	}
	if file.Sessions == nil {
		file.Sessions = map[string]SessionAuth{}
	}
	return file, nil
}

func ConfigForSession(path, sessionID string) (Config, error) {
	auth, err := LoadSessionAuth(path, sessionID)
	if err != nil {
		return Config{}, err
	}
	return Config{DriveRoot: auth.DriveRoot, MountPath: auth.MountPath, AuthConfigPath: path, SessionID: auth.SessionID}, nil
}

func LoadSessionAuth(path, sessionID string) (SessionAuth, error) {
	if strings.TrimSpace(sessionID) == "" {
		return SessionAuth{}, fmt.Errorf("session id is required")
	}
	file, err := LoadAuthFile(path)
	if err != nil {
		return SessionAuth{}, err
	}
	auth, ok := file.Sessions[sessionID]
	if !ok {
		return SessionAuth{}, fmt.Errorf("session %q is not configured in %s", sessionID, path)
	}
	if err := auth.Validate(); err != nil {
		return SessionAuth{}, err
	}
	return auth, nil
}

func UpsertSessionAuth(path string, auth SessionAuth) error {
	if err := auth.Validate(); err != nil {
		return err
	}
	file := AuthFile{SchemaVersion: AuthSchemaVersion, Sessions: map[string]SessionAuth{}}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &file); err != nil {
			return fmt.Errorf("parse existing Colab auth config: %w", err)
		}
		if file.SchemaVersion != AuthSchemaVersion {
			return fmt.Errorf("auth schema_version must be %q", AuthSchemaVersion)
		}
		if file.Sessions == nil {
			file.Sessions = map[string]SessionAuth{}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	file.Sessions[auth.SessionID] = auth
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func (auth SessionAuth) Validate() error {
	if strings.TrimSpace(auth.SessionID) == "" {
		return fmt.Errorf("session_id is required")
	}
	if strings.TrimSpace(auth.DriveRoot) == "" {
		return fmt.Errorf("drive_root is required for session %q", auth.SessionID)
	}
	if strings.TrimSpace(auth.MountPath) == "" {
		return fmt.Errorf("mount_path is required for session %q", auth.SessionID)
	}
	if auth.ColabCredentialFile == "" && auth.ColabRefreshTokenEnv == "" {
		return fmt.Errorf("session %q needs Colab credential file or refresh-token environment variable", auth.SessionID)
	}
	if auth.DriveCredentialFile == "" && auth.DriveRefreshTokenEnv == "" {
		return fmt.Errorf("session %q needs Drive credential file or refresh-token environment variable", auth.SessionID)
	}
	return nil
}
