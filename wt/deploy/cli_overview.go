package main

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// overviewApp is the part of app.json the bare wt overview reports.
type overviewApp struct {
	Name string `json:"name"`
	Host string `json:"host"`
}

// appSummary describes the app.json in dir; hasApp reports whether one could be read.
func appSummary(dir string) (summary string, hasApp bool) {
	path := filepath.Join(dir, "app.json")

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "app: none in this directory", false
	}

	if err != nil {
		return fmt.Sprintf("app: %v", err), false
	}

	var app overviewApp
	if err := json.Unmarshal(data, &app); err != nil {
		return fmt.Sprintf("app: app.json: %v", err), false
	}

	name := app.Name
	if name == "" {
		name = filepath.Base(dir)
	}

	if app.Host != "" {
		return fmt.Sprintf("app: %s (host %s)", name, app.Host), true
	}

	return "app: " + name, true
}

// overviewHint suggests the most useful next command for the current directory and host.
func overviewHint(hasApp, hasHost bool) string {
	switch {
	case hasApp && hasHost:
		return "run `wt help` for commands; `wt deploy` deploys this app"
	case hasApp:
		return "run `wt help` for commands; `wt dev` runs this app locally"
	default:
		return "run `wt help` for commands; `wt new --dir PATH` scaffolds an app"
	}
}

// printOverview reports the current directory, local app, host and config file for a bare wt call.
func printOverview(w io.Writer, dir string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}

	fmt.Fprintf(w, "wt: no command given\ndirectory: %s\n", abs)

	summary, hasApp := appSummary(abs)
	fmt.Fprintln(w, summary)

	host := firstNonEmpty(os.Getenv("WT_DEPLOY_HOST"))
	configPath := defaultConfigPath()

	cfg, cfgErr := loadFileConfig("")
	if cfgErr == nil {
		host = firstNonEmpty(host, cfg.Host)
	}

	if host == "" {
		fmt.Fprintln(w, "host: not configured (run `wt auth login`, or set --host/WT_DEPLOY_HOST)")
	} else {
		fmt.Fprintf(w, "host: %s\n", host)
	}

	if configPath != "" {
		if cfgErr != nil {
			fmt.Fprintf(w, "config: %s (%v)\n", configPath, cfgErr)
		} else {
			fmt.Fprintf(w, "config: %s\n", configPath)
		}
	}

	fmt.Fprintln(w, overviewHint(hasApp, host != ""))
}
