package main

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// fileProfile is one named deploy target: its supervisor, token and optional app-host suffix.
type fileProfile struct {
	Host      string `json:"host,omitempty"`
	Token     string `json:"token,omitempty"`
	AppDomain string `json:"app_domain,omitempty"`
}

// fileConfig is the optional ~/.config/wt/config.json shared by CLI commands.
type fileConfig struct {
	Host      string                 `json:"host,omitempty"`
	Token     string                 `json:"token,omitempty"`
	Profile   string                 `json:"profile,omitempty"`
	Profiles  map[string]fileProfile `json:"profiles,omitempty"`
	Wasmtime  string                 `json:"wasmtime,omitempty"`
	Target    string                 `json:"target,omitempty"`
	AppHost   string                 `json:"app_host,omitempty"`
	AppDomain string                 `json:"app_domain,omitempty"`
}

// flagValueArg finds the last value of a global flag before parsing, matching the flag package's repeated-flag behaviour.
func flagValueArg(args []string, name string) string {
	flagName := "--" + name
	value := ""

	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			break
		}

		if arg == flagName {
			if index+1 < len(args) {
				value = args[index+1]
				index++
			}

			continue
		}

		if candidate, ok := strings.CutPrefix(arg, flagName+"="); ok {
			value = candidate
		}
	}

	return value
}

// configArg finds --config before flag parsing so file defaults can seed the flag values.
func configArg(args []string) string {
	return flagValueArg(args, "config")
}

// profileArg finds --profile before flag parsing so a profile can seed the flag values.
func profileArg(args []string) string {
	return flagValueArg(args, "profile")
}

// activeProfileName resolves the profile from a flag, WT_PROFILE and the config default.
func activeProfileName(flagValue string, cfg fileConfig) string {
	return firstNonEmpty(flagValue, os.Getenv("WT_PROFILE"), cfg.Profile)
}

// profileNames lists the configured profiles in a stable order.
func profileNames(cfg fileConfig) []string {
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

// resolveProfile returns the selected profile, or the top-level host and token when no profile is selected.
func resolveProfile(cfg fileConfig, name string) (fileProfile, error) {
	if name == "" {
		return fileProfile{Host: cfg.Host, Token: cfg.Token, AppDomain: cfg.AppDomain}, nil
	}

	profile, ok := cfg.Profiles[name]
	if !ok {
		names := profileNames(cfg)
		if len(names) == 0 {
			return fileProfile{}, fmt.Errorf("unknown profile %q; no profiles are configured", name)
		}

		return fileProfile{}, fmt.Errorf("unknown profile %q; configured profiles: %s", name, strings.Join(names, ", "))
	}

	return profile, nil
}

// defaultConfigPath returns WT_CONFIG, then the XDG config path, then ~/.config/wt/config.json.
func defaultConfigPath() string {
	if value := strings.TrimSpace(os.Getenv("WT_CONFIG")); value != "" {
		return value
	}

	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "wt", "config.json")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return filepath.Join(home, ".config", "wt", "config.json")
}

func loadFileConfig(explicit string) (fileConfig, error) {
	path := explicit
	if path == "" {
		path = defaultConfigPath()
	}

	if path == "" {
		return fileConfig{}, nil
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return fileConfig{}, nil
	}

	if err != nil {
		return fileConfig{}, fmt.Errorf("config %s: %w", path, err)
	}

	var cfg fileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fileConfig{}, fmt.Errorf("config %s: %w", path, err)
	}

	warnInsecureConfig(path, cfg.Token)

	return cfg, nil
}
