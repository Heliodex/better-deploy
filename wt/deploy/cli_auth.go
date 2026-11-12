package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// configPrivateMask is the group and other permission bits that must stay clear in a token-bearing config.
const configPrivateMask = 0o077

// authStatusReport is the token-free view printed by wt auth status.
type authStatusReport struct {
	Host          string `json:"host,omitempty"`
	HostSource    string `json:"host_source,omitempty"`
	TokenSource   string `json:"token_source,omitempty"`
	Profile       string `json:"profile,omitempty"`
	ProfileSource string `json:"profile_source,omitempty"`
	AppDomain     string `json:"app_domain,omitempty"`
	Config        string `json:"config,omitempty"`
	ConfigMode    string `json:"config_mode,omitempty"`
	ConfigPrivate bool   `json:"config_private"`
	Check         string `json:"check,omitempty"`
}

// normaliseSupervisorHost adds https when needed and rejects non-HTTP schemes.
func normaliseSupervisorHost(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", errors.New("supervisor URL is required")
	}

	candidate := strings.TrimSuffix(host, "/")
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}

	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid supervisor URL %q", host)
	}

	return candidate, nil
}

// validateAuth checks that the supervisor accepts the token before it is saved.
func validateAuth(host, token string, timeout time.Duration) error {
	status, _, body, err := apiDo(
		clientOptions{host: host, token: token, timeout: timeout},
		http.MethodGet,
		"/capabilities",
		nil,
		"",
	)
	if err != nil {
		return err
	}

	return checkAPI(status, body)
}

// writeAuthReport prints the token-free auth status, as JSON when requested.
func writeAuthReport(out io.Writer, report authStatusReport, jsonOut bool) error {
	if !jsonOut {
		if report.Profile != "" {
			if report.ProfileSource != "" {
				fmt.Fprintf(out, "profile: %s (%s)\n", report.Profile, report.ProfileSource)
			} else {
				fmt.Fprintf(out, "profile: %s\n", report.Profile)
			}
		}

		host := report.Host
		if host == "" {
			host = "not configured"
		}

		if report.HostSource != "" && report.HostSource != "none" {
			fmt.Fprintf(out, "host: %s (%s)\n", host, report.HostSource)
		} else {
			fmt.Fprintf(out, "host: %s\n", host)
		}

		token := report.TokenSource
		if token == "" || token == "none" {
			token = "not configured"
		}

		fmt.Fprintf(out, "token: %s\n", token)

		if report.AppDomain != "" {
			fmt.Fprintf(out, "app domain: %s\n", report.AppDomain)
		}

		if report.Config != "" {
			if report.ConfigMode != "" {
				fmt.Fprintf(out, "config: %s (%s)\n", report.Config, report.ConfigMode)
			} else {
				fmt.Fprintf(out, "config: %s (not created)\n", report.Config)
			}
		}

		if report.Check != "" {
			fmt.Fprintf(out, "check: %s\n", report.Check)
		}

		return nil
	}

	data, err := json.Marshal(report)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, string(data))

	return nil
}

// noteHostSource fills the credential value and a human-readable source for auth status.
func noteHostSource(values clientFlagValues, profileName string, profile fileProfile) (string, string) {
	switch {
	case strings.TrimSpace(values.host) != "":
		return strings.TrimSpace(values.host), "command line"
	case strings.TrimSpace(os.Getenv("WT_DEPLOY_HOST")) != "":
		return strings.TrimSpace(os.Getenv("WT_DEPLOY_HOST")), "environment"
	case strings.TrimSpace(profile.Host) != "":
		if profileName != "" {
			return strings.TrimSpace(profile.Host), "profile " + profileName
		}

		return strings.TrimSpace(profile.Host), "config file"
	default:
		return "", "none"
	}
}

// noteTokenSource fills the token and a human-readable source without returning the token itself in status output.
func noteTokenSource(values clientFlagValues, profileName string, profile fileProfile) (string, string) {
	switch {
	case strings.TrimSpace(values.token) != "":
		return strings.TrimSpace(values.token), "command line"
	case strings.TrimSpace(os.Getenv("WT_TOKEN")) != "":
		return strings.TrimSpace(os.Getenv("WT_TOKEN")), "environment"
	case strings.TrimSpace(profile.Token) != "":
		if profileName != "" {
			return strings.TrimSpace(profile.Token), "profile " + profileName
		}

		return strings.TrimSpace(profile.Token), "config file"
	default:
		return "", "none"
	}
}

// noteProfileSource reports where the selected profile came from, when one is selected.
func noteProfileSource(values clientFlagValues, cfg fileConfig) string {
	switch {
	case strings.TrimSpace(values.profile) != "":
		return "command line"
	case strings.TrimSpace(os.Getenv("WT_PROFILE")) != "":
		return "environment"
	case strings.TrimSpace(cfg.Profile) != "":
		return "config file"
	default:
		return ""
	}
}

// loginProfile returns the saved settings a login starts from: the top-level settings when no profile is named, otherwise that profile.
func loginProfile(existing fileConfig, profileName string) fileProfile {
	if profileName == "" {
		return fileProfile{Host: existing.Host, Token: existing.Token, AppDomain: existing.AppDomain}
	}

	return existing.Profiles[profileName]
}

// loginHost returns the normalised supervisor URL from --host, WT_DEPLOY_HOST, the saved profile or a prompt.
func loginHost(input *authInput, flagHost string, profile fileProfile) (string, error) {
	host := firstNonEmpty(flagHost, os.Getenv("WT_DEPLOY_HOST"), profile.Host)
	if strings.TrimSpace(host) == "" {
		var err error

		host, err = input.line("Supervisor URL", profile.Host)
		if err != nil {
			return "", err
		}
	}

	return normaliseSupervisorHost(host)
}

// loginToken reads the token from stdin, or prompts for it with hidden input when a terminal is attached.
func loginToken(input *authInput, tokenStdin bool) (string, error) {
	if !tokenStdin && input.interactive {
		return input.secret("Token: ")
	}

	return input.line("Token", "")
}

// runAuthLogin stores a host and token after checking that the supervisor accepts them.
func runAuthLogin(args []string, in io.Reader, out, errOut io.Writer) error {
	values, rest, err := extractClientFlags(args)
	if err != nil {
		return err
	}

	configPath := authConfigPath(values.config)
	if configPath == "" {
		return errors.New("cannot determine config file; set --config or HOME/XDG_CONFIG_HOME")
	}

	existing, err := loadFileConfig(values.config)
	if err != nil {
		return err
	}

	profileName := activeProfileName(values.profile, existing)
	profile := loginProfile(existing, profileName)

	loginFlags := newFlagSet("auth login")
	tokenStdin := loginFlags.Bool("token-stdin", false, "read the token from stdin")
	appDomain := loginFlags.String("app-domain", profile.AppDomain, "app host suffix for <app>.localhost hosts")

	if err := loginFlags.Parse(rest); err != nil {
		return err
	}

	if loginFlags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", loginFlags.Arg(0))
	}

	if values.token != "" {
		return errors.New("--token is not accepted by auth login; use --token-stdin or pipe the token")
	}

	input := newAuthInput(in, errOut)

	host, err := loginHost(input, values.host, profile)
	if err != nil {
		return err
	}

	token, err := loginToken(input, *tokenStdin)
	if err != nil {
		return err
	}

	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("deploy token is required")
	}

	if err := validateAuth(host, token, values.timeout); err != nil {
		return fmt.Errorf("not saved: %w", err)
	}

	if err := saveAuthConfig(configPath, profileName, host, token, strings.TrimSpace(*appDomain)); err != nil {
		return err
	}

	if profileName != "" {
		fmt.Fprintf(out, "saved supervisor URL and token to %s (profile %s)\n", configPath, profileName)
	} else {
		fmt.Fprintf(out, "saved supervisor URL and token to %s\n", configPath)
	}

	return nil
}

// runAuthStatus reports where the current host and token come from, optionally checking them.
func runAuthStatus(args []string, out io.Writer) error {
	values, rest, err := extractClientFlags(args)
	if err != nil {
		return err
	}

	statusFlags := newFlagSet("auth status")
	check := statusFlags.Bool("check", false, "check the host and token against the supervisor")

	if err := statusFlags.Parse(rest); err != nil {
		return err
	}

	if statusFlags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", statusFlags.Arg(0))
	}

	cfg, err := loadFileConfig(values.config)
	if err != nil {
		return err
	}

	profileName := activeProfileName(values.profile, cfg)

	profile, err := resolveProfile(cfg, profileName)
	if err != nil {
		return err
	}

	host, hostSource := noteHostSource(values, profileName, profile)
	token, tokenSource := noteTokenSource(values, profileName, profile)
	configPath := authConfigPath(values.config)
	mode, private := configMode(configPath)

	report := authStatusReport{
		Host:          host,
		HostSource:    hostSource,
		TokenSource:   tokenSource,
		Profile:       profileName,
		ProfileSource: noteProfileSource(values, cfg),
		AppDomain:     profile.AppDomain,
		Config:        configPath,
		ConfigMode:    mode,
		ConfigPrivate: private,
	}

	if *check {
		if host == "" {
			return errors.New("no supervisor URL configured; run wt auth login")
		}

		if token == "" {
			return errors.New("no token configured; run wt auth login")
		}

		if err := validateAuth(host, token, values.timeout); err != nil {
			return fmt.Errorf("check: %w", err)
		}

		report.Check = "ok"
	}

	return writeAuthReport(out, report, values.jsonOut)
}

// runAuthLogout removes the stored token and leaves the host and other config fields alone.
func runAuthLogout(args []string, out io.Writer) error {
	values, rest, err := extractClientFlags(args)
	if err != nil {
		return err
	}

	logoutFlags := newFlagSet("auth logout")

	if err := logoutFlags.Parse(rest); err != nil {
		return err
	}

	if logoutFlags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", logoutFlags.Arg(0))
	}

	configPath := authConfigPath(values.config)
	if configPath == "" {
		return errors.New("cannot determine config file; set --config or HOME/XDG_CONFIG_HOME")
	}

	cfg, err := loadFileConfig(values.config)
	if err != nil {
		return err
	}

	profileName := activeProfileName(values.profile, cfg)

	removed, err := removeAuthToken(configPath, profileName)
	if err != nil {
		return err
	}

	if !removed {
		if profileName != "" {
			fmt.Fprintf(out, "no token stored in %s for profile %s\n", configPath, profileName)
		} else {
			fmt.Fprintf(out, "no token stored in %s\n", configPath)
		}

		return nil
	}

	if profileName != "" {
		fmt.Fprintf(out, "removed token from %s (profile %s)\n", configPath, profileName)
	} else {
		fmt.Fprintf(out, "removed token from %s\n", configPath)
	}

	return nil
}

// runAuthUse selects the profile that commands use when --profile is not given.
func runAuthUse(args []string, out io.Writer) error {
	values, rest, err := extractClientFlags(args)
	if err != nil {
		return err
	}

	if len(rest) != 1 {
		return errors.New("usage: wt auth use <profile>")
	}

	name := strings.TrimSpace(rest[0])
	if name == "" {
		return errors.New("profile name is required")
	}

	cfg, err := loadFileConfig(values.config)
	if err != nil {
		return err
	}

	if _, ok := cfg.Profiles[name]; !ok {
		names := profileNames(cfg)
		if len(names) == 0 {
			return errors.New("no profiles are configured; run wt auth login --profile <name>")
		}

		return fmt.Errorf("unknown profile %q; configured profiles: %s", name, strings.Join(names, ", "))
	}

	configPath := authConfigPath(values.config)
	if configPath == "" {
		return errors.New("cannot determine config file; set --config or HOME/XDG_CONFIG_HOME")
	}

	if err := setDefaultProfile(configPath, name); err != nil {
		return err
	}

	fmt.Fprintf(out, "using profile %s from %s\n", name, configPath)

	return nil
}

// runAuthWithIO dispatches auth actions with injectable input and output for tests.
func runAuthWithIO(args []string, in io.Reader, out, errOut io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: wt auth <login|status|logout|use>")
	}

	action, rest := args[0], args[1:]

	switch action {
	case "login":
		return runAuthLogin(rest, in, out, errOut)
	case "status":
		return runAuthStatus(rest, out)
	case "logout":
		return runAuthLogout(rest, out)
	case "use":
		return runAuthUse(rest, out)
	default:
		return fmt.Errorf("unknown auth action %q", action)
	}
}

// runAuth dispatches wt auth with the process standard streams.
func runAuth(args []string) error {
	return runAuthWithIO(args, os.Stdin, os.Stdout, os.Stderr)
}
