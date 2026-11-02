package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
)

// runOpen prints the app URL and opens it with the platform handler unless --print is set.
func runOpen(args []string) error {
	cfg, rest, err := clientFlags("open", args, true)
	if err != nil {
		return err
	}
	printOnly := false
	for index := 1; index < len(rest); index++ {
		if rest[index] == "--print" {
			printOnly = true
			continue
		}
		return fmt.Errorf("unexpected argument %q", rest[index])
	}
	name, err := appNameArg(rest, "open")
	if err != nil {
		return err
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
	scheme := "http"
	if strings.HasPrefix(baseURL(cfg.host), "https://") {
		scheme = "https"
	}
	url := scheme + "://" + app.Host
	fmt.Println(url)
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
	return exec.Command(command, append(commandArgs, url)...).Start()
}
