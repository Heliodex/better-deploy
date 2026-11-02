package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// appURLForHost builds the browser URL for an app host, adding the supervisor's port for local listeners.
func appURLForHost(supervisorHost, appHost string) string {
	scheme := "http"
	parsed, err := url.Parse(baseURL(supervisorHost))
	if err == nil {
		if parsed.Scheme == "https" {
			scheme = "https"
		}
		if parsed.Port() != "" && !strings.Contains(appHost, ":") {
			appHost += ":" + parsed.Port()
		}
	}
	return scheme + "://" + appHost
}

// runOpen prints the app URL and opens it with the platform handler unless --print is set.
func runOpen(args []string) error {
	cfg, rest, err := clientFlags(args, true)
	if err != nil {
		return err
	}
	printOnly := false
	name := ""
	for _, value := range rest {
		switch {
		case value == "--print":
			printOnly = true
		case strings.HasPrefix(value, "--print="):
			parsed, err := strconv.ParseBool(strings.TrimPrefix(value, "--print="))
			if err != nil {
				return fmt.Errorf("--print: %w", err)
			}
			printOnly = parsed
		case name == "":
			name = value
		default:
			return fmt.Errorf("unexpected argument %q", value)
		}
	}
	if name == "" {
		return fmt.Errorf("usage: wt open <app>")
	}
	status, _, body, err := apiDo(cfg, http.MethodGet, "/apps/"+name, nil, "")
	if err != nil {
		return err
	}
	if err := checkAPI(status, body); err != nil {
		return err
	}
	var app struct {
		Host string `json:"host"`
	}
	if err := json.Unmarshal(body, &app); err != nil {
		return err
	}
	if app.Host == "" {
		return fmt.Errorf("app has no host")
	}
	target := appURLForHost(cfg.host, app.Host)
	fmt.Println(target)
	if printOnly {
		return nil
	}
	var command string
	var commandArgs []string
	switch runtime.GOOS {
	case "darwin":
		command = "open"
	case "windows":
		command = "rundll32"
		commandArgs = []string{"url.dll,FileProtocolHandler"}
	default:
		command = "xdg-open"
	}
	return exec.Command(command, append(commandArgs, target)...).Start()
}
