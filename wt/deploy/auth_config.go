package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// authConfigPath returns the explicit config path, or the default XDG path.
func authConfigPath(explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}

	return defaultConfigPath()
}

// configMap reads a config file while keeping unknown fields so an update does not drop them.
func configMap(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}

	values := map[string]any{}
	if len(bytes.TrimSpace(data)) == 0 {
		return values, nil
	}

	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}

	if values == nil {
		values = map[string]any{}
	}

	return values, nil
}

// marshalConfig writes a readable config with two-space indentation.
func marshalConfig(values map[string]any) ([]byte, error) {
	var buf bytes.Buffer

	encoder := jsontext.NewEncoder(&buf, jsontext.WithIndent("  "))
	if err := json.MarshalEncode(encoder, values); err != nil {
		return nil, err
	}

	buf.WriteByte('\n')

	return buf.Bytes(), nil
}

// writeConfigMap atomically replaces a config file with private permissions.
func writeConfigMap(path string, values map[string]any) error {
	data, err := marshalConfig(values)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".wt-config-*")
	if err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}

	tmpName := tmp.Name()
	closed := false

	defer func() {
		if !closed {
			_ = tmp.Close()
		}

		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}

	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}

	closed = true

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}

	tmpName = ""

	return nil
}

// objectMap returns a nested config object, creating it when it does not exist.
func objectMap(parent map[string]any, key string) (map[string]any, error) {
	value, ok := parent[key]
	if !ok || value == nil {
		child := map[string]any{}
		parent[key] = child

		return child, nil
	}

	child, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("config field %q is not an object", key)
	}

	return child, nil
}

// saveAuthConfig stores the host, token and optional app domain without dropping unrelated config fields.
func saveAuthConfig(path, profile, host, token, appDomain string) error {
	values, err := configMap(path)
	if err != nil {
		return err
	}

	target := values

	if profile != "" {
		profiles, err := objectMap(values, "profiles")
		if err != nil {
			return err
		}

		target, err = objectMap(profiles, profile)
		if err != nil {
			return err
		}
	}

	target["host"] = host
	target["token"] = token

	if appDomain != "" {
		target["app_domain"] = appDomain
	} else {
		delete(target, "app_domain")
	}

	return writeConfigMap(path, values)
}

// removeAuthToken deletes the token from the selected profile while keeping its host and every other config field.
func removeAuthToken(path, profile string) (bool, error) {
	values, err := configMap(path)
	if err != nil {
		return false, err
	}

	target := values

	if profile != "" {
		profiles, ok := values["profiles"].(map[string]any)
		if !ok {
			return false, nil
		}

		target, ok = profiles[profile].(map[string]any)
		if !ok {
			return false, nil
		}
	}

	if _, ok := target["token"]; !ok {
		return false, nil
	}

	delete(target, "token")

	return true, writeConfigMap(path, values)
}

// setDefaultProfile records which profile commands use when --profile is not given.
func setDefaultProfile(path, profile string) error {
	values, err := configMap(path)
	if err != nil {
		return err
	}

	values["profile"] = profile

	return writeConfigMap(path, values)
}

// configMode returns the octal mode and whether only the owner can read the file.
func configMode(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}

	perm := info.Mode().Perm()

	private := perm&configPrivateMask == 0
	if runtime.GOOS == "windows" {
		private = true
	}

	return fmt.Sprintf("%04o", perm), private
}

// warnInsecureConfig prints one warning when a token-bearing config is readable by other users.
func warnInsecureConfig(path, token string) {
	if token == "" || runtime.GOOS == "windows" {
		return
	}

	mode, private := configMode(path)
	if mode == "" || private {
		return
	}

	fmt.Fprintf(
		os.Stderr,
		"wt-deploy: warning: %s contains a token and has mode %s; run chmod 600 %s\n",
		path,
		mode,
		path,
	)
}
