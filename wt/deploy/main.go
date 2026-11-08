// Command wt-deploy builds a WASI HTTP component, precompiles it with wasmtime, and uploads it to a wt-supervisor.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

type deployConfig struct {
	dir, app, host, token, wasmtime, target, appHost, appDomain, profile, out string
	buildComponentCommand                                                     string
	skipBuild, skipCompile, skipStatic, jsonOut, forceBuild, skipRouteCheck   bool
	overlayEnv                                                                bool
	targetConfigured                                                          bool
	verify                                                                    bool
	verifyPath                                                                string
	verifyTimeout                                                             time.Duration
	envVars                                                                   []string
	start                                                                     time.Time
}

func env(key string) string {
	return os.Getenv(key)
}

// resolveConfig builds the deploy config from the config file, environment and deploy flags.
func resolveConfig(args []string) (*deployConfig, error) {
	configPath := configArg(args)

	fileCfg, err := loadFileConfig(configPath)
	if err != nil {
		return nil, err
	}

	profileName := activeProfileName(profileArg(args), fileCfg)

	profile, err := resolveProfile(fileCfg, profileName)
	if err != nil {
		return nil, err
	}

	flags := newFlagSet("deploy")
	dir := flags.String("dir", ".", "app directory containing app.json")
	app := flags.String("app", "", "app name (default: app.json name or directory name)")
	profileFlag := flags.String("profile", profileName, "config profile for host, token and app domain")
	host := flags.String("host", firstNonEmpty(env("WT_DEPLOY_HOST"), profile.Host), "supervisor URL, e.g. http://127.0.0.1:8080")
	token := flags.String("token", firstNonEmpty(env("WT_TOKEN"), profile.Token), "deploy bearer token")
	wasmtime := flags.String("wasmtime", firstNonEmpty(env("WT_WASMTIME"), fileCfg.Wasmtime, "wasmtime"), "wasmtime binary")
	targetDefault := firstNonEmpty(env("WT_WASMTIME_TARGET"), fileCfg.Target)

	targetConfigured := targetDefault != ""
	if targetDefault == "" {
		targetDefault = "x86_64-unknown-linux-musl"
	}

	target := flags.String("target", targetDefault, "wasmtime compile target (default: supervisor target or x86_64-unknown-linux-musl)")
	appDomain := flags.String("app-domain", profile.AppDomain, "app host suffix for <app>.localhost hosts")
	appHost := flags.String("app-host", fileCfg.AppHost, "override the app host in the uploaded app.json")
	config := flags.String("config", configPath, "config file (default ~/.config/wt/config.json)")
	jsonOut := flags.Bool("json", false, "print only the raw API response")
	forceBuild := flags.Bool("force-build", false, "rebuild the component and static phases even when their inputs are unchanged")
	skipRouteCheck := flags.Bool("skip-route-check", false, "do not verify that the app host is already routed")
	skipBuild := flags.Bool("skip-build", false, "skip running app.json's build commands")
	verify := flags.Bool("verify", false, "probe the public app host until the fresh deployment serves")
	verifyPath := flags.String("verify-path", defaultRoutePath, "public path to probe for --verify")
	verifyTimeout := flags.Duration("verify-timeout", verifyDefaultTimeout, "how long --verify waits for the app to serve")
	envVars := deployEnvFlag(flags)

	skipCompile := flags.Bool("skip-compile", false, "do not compile; include main.cwasm only if it already exists")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}

	profileSet := false
	appDomainSet := false

	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "target":
			targetConfigured = true
		case "profile":
			profileSet = true
		case "app-domain":
			appDomainSet = true
		}
	})

	if *target != targetDefault {
		targetConfigured = true
	}

	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	if *config != configPath {
		fileCfg, err = loadFileConfig(*config)
		if err != nil {
			return nil, err
		}

		if !profileSet {
			*profileFlag = activeProfileName("", fileCfg)
		}

		profile, err = resolveProfile(fileCfg, *profileFlag)
		if err != nil {
			return nil, err
		}

		if *host == "" {
			*host = profile.Host
		}

		if *token == "" {
			*token = profile.Token
		}

		if !appDomainSet {
			*appDomain = profile.AppDomain
		}
	}

	if *host == "" {
		return nil, errors.New("set --host, WT_DEPLOY_HOST, or host in the config file")
	}

	if !appDomainSet {
		*appDomain = appDomainForTarget(profile.Host, *host, *appDomain)
	}

	return &deployConfig{
		dir:              *dir,
		app:              *app,
		host:             *host,
		token:            *token,
		wasmtime:         *wasmtime,
		target:           *target,
		skipBuild:        *skipBuild,
		skipCompile:      *skipCompile,
		forceBuild:       *forceBuild,
		skipRouteCheck:   *skipRouteCheck,
		appHost:          *appHost,
		appDomain:        *appDomain,
		profile:          strings.TrimSpace(*profileFlag),
		jsonOut:          *jsonOut,
		targetConfigured: targetConfigured,
		verify:           *verify,
		verifyPath:       *verifyPath,
		verifyTimeout:    *verifyTimeout,
		envVars:          *envVars,
	}, nil
}

// resolveBundleConfig parses the flags for building a bundle without a supervisor.
func resolveBundleConfig(args []string) (*deployConfig, error) {
	configPath := configArg(args)

	fileCfg, err := loadFileConfig(configPath)
	if err != nil {
		return nil, err
	}

	profileName := activeProfileName(profileArg(args), fileCfg)

	profile, err := resolveProfile(fileCfg, profileName)
	if err != nil {
		return nil, err
	}

	flags := newFlagSet("bundle")
	dir := flags.String("dir", ".", "app directory containing app.json")
	wasmtime := flags.String("wasmtime", firstNonEmpty(env("WT_WASMTIME"), fileCfg.Wasmtime, "wasmtime"), "wasmtime binary")
	target := flags.String("target", firstNonEmpty(env("WT_WASMTIME_TARGET"), fileCfg.Target, "x86_64-unknown-linux-musl"), "wasmtime compile target")
	out := flags.String("out", "", "output file (default <name>.tar.gz)")
	profileFlag := flags.String("profile", profileName, "config profile for the app domain")
	appDomain := flags.String("app-domain", profile.AppDomain, "app host suffix for <app>.localhost hosts")
	flags.String("config", configPath, "config file (default ~/.config/wt/config.json)")
	skipBuild := flags.Bool("skip-build", false, "skip running app.json's build commands")
	skipCompile := flags.Bool("skip-compile", false, "do not compile; include main.cwasm only if it already exists")

	forceBuild := flags.Bool("force-build", false, "rebuild the component and static phases even when their inputs are unchanged")

	if err := flags.Parse(args); err != nil {
		return nil, err
	}

	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	return &deployConfig{
		dir:         *dir,
		wasmtime:    *wasmtime,
		target:      *target,
		profile:     strings.TrimSpace(*profileFlag),
		appDomain:   *appDomain,
		skipBuild:   *skipBuild,
		skipCompile: *skipCompile,
		forceBuild:  *forceBuild,
		out:         *out,
	}, nil
}

// runBundle builds the deploy artifact into a file instead of uploading it, for CI or manual upload.
func runBundle(args []string) error {
	cfg, err := resolveBundleConfig(args)
	if err != nil {
		return err
	}

	m, data, status, err := buildBundle(cfg)
	if err != nil {
		return err
	}

	printBuildStatus(status)

	out := cfg.out
	if out == "" {
		out = m.Name + ".tar.gz"
	}

	if err := os.WriteFile(out, data, privateFileMode); err != nil {
		return fmt.Errorf("write bundle: %w", err)
	}

	fmt.Println("wrote", out)

	return nil
}

// isHelpFlag reports whether arg asks for help, including the single-dash -help form that Go's flag package also accepts.
func isHelpFlag(arg string) bool {
	return arg == "-h" || arg == "-help" || arg == "--help"
}

// withoutHelpFlags drops help flags from a help command's topic arguments.
func withoutHelpFlags(args []string) []string {
	topic := make([]string, 0, len(args))
	for _, arg := range args {
		if !isHelpFlag(arg) {
			topic = append(topic, arg)
		}
	}

	return topic
}

func main() {
	command := "deploy"

	args := os.Args[1:]
	if len(args) == 0 {
		printOverview(os.Stdout, ".")
		return
	}

	hasCommand := !strings.HasPrefix(args[0], "-")
	if hasCommand {
		command, args = args[0], args[1:]
	} else if isHelpFlag(args[0]) {
		command, args = "help", args[1:]
	}

	helpRequested := command != "help" && slices.ContainsFunc(args, isHelpFlag)

	var err error

	switch {
	case command == "help" || command == "-h" || command == "--help":
		err = writeHelp(os.Stdout, withoutHelpFlags(args))
	case helpRequested:
		err = writeHelp(os.Stdout, helpTarget(command, args))
	default:
		err = runCommand(command, args)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "wt-deploy:", err)
		os.Exit(1)
	}
}

// runCommand dispatches one top-level command.
func runCommand(command string, args []string) error {
	switch command {
	case "deploy":
		return deploy(args)
	case "bundle", "build":
		return runBundle(args)
	case "dev":
		return runDev(args)
	case "new":
		return runNew(args)
	case "contract":
		return runContract(args)
	case "apps":
		return runApps(args)
	case "logs":
		return runLogs(args)
	case "jobs":
		return runJobs(args)
	case "blobs":
		return runBlobs(args)
	case "secrets":
		return runSecrets(args)
	case "route":
		return runRoute(args)
	case "doctor":
		return runDoctor(args)
	case "auth":
		return runAuth(args)
	case "runtime":
		return runRuntime(args)
	case "inspect":
		return runInspect(args)
	case "rollback":
		return runRollback(args)
	case "backup":
		return runBackup(args)
	case "config":
		return runConfig(args)
	case "open":
		return runOpen(args)
	case "completion":
		return runCompletion(args)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}
