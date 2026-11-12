package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultClientTimeout = 30 * time.Second

type clientOptions struct {
	host, token, profile, appDomain string
	jsonOut                         bool
	timeout                         time.Duration
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}

	return ""
}

func runApps(args []string) error {
	cfg, rest, err := clientFlags(args, true)
	if err != nil {
		return err
	}

	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q", rest[0])
	}

	if !cfg.jsonOut {
		capClient := clientOptions{host: cfg.host, token: cfg.token, timeout: 5 * time.Second}
		if caps, err := fetchCapabilities(capClient); err == nil {
			fmt.Fprintln(os.Stderr, caps.summary())
		}
	}

	status, _, body, err := apiDo(cfg, http.MethodGet, "/apps", nil, "")
	if err != nil {
		return err
	}

	if err := checkAPI(status, body); err != nil {
		return err
	}

	if cfg.jsonOut {
		printRawOrJSON(cfg, body)
		return nil
	}

	var apps []struct {
		Name          string  `json:"name"`
		Host          string  `json:"host"`
		Status        string  `json:"status"`
		Ready         bool    `json:"ready"`
		PID           int     `json:"pid"`
		CurrentDigest string  `json:"currentDigest"`
		CPUPercent    float64 `json:"cpuPercent"`
		MemoryBytes   int64   `json:"memoryBytes"`
	}
	if err := json.Unmarshal(body, &apps); err != nil {
		return err
	}

	if len(apps) == 0 {
		fmt.Println("no apps")
		return nil
	}

	fmt.Printf("%-24s %-32s %-10s %-6s %-7s %s\n", "NAME", "HOST", "STATUS", "READY", "PID", "DIGEST")

	for _, app := range apps {
		digest := app.CurrentDigest
		if len(digest) > shortHashLength {
			digest = digest[:shortHashLength]
		}

		fmt.Printf("%-24s %-32s %-10s %-6v %-7d %s\n", app.Name, app.Host, app.Status, app.Ready, app.PID, digest)
	}

	return nil
}

func runRollback(args []string) error {
	cfg, rest, err := clientFlags(args, true)
	if err != nil {
		return err
	}

	if len(rest) < 2 {
		return errors.New("usage: wt rollback <app> <deployment-id> [--backup SNAPSHOT]")
	}

	backup := ""

	for index := 2; index < len(rest); index++ {
		if rest[index] == "--backup" && index+1 < len(rest) {
			index++
			backup = rest[index]

			continue
		}

		if value, ok := strings.CutPrefix(rest[index], "--backup="); ok {
			backup = value
			continue
		}

		return fmt.Errorf("unexpected argument %q", rest[index])
	}

	path := "/apps/" + rest[0] + "/rollback/" + rest[1]
	if backup != "" {
		path += "?backup=" + backup
	}

	status, _, body, err := apiDo(cfg, http.MethodPost, path, nil, "")
	if err != nil {
		return err
	}

	if err := checkAPI(status, body); err != nil {
		return err
	}

	printRawOrJSON(cfg, body)

	return nil
}
