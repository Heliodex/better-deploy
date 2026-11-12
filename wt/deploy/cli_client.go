package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// clientFlagValues holds the global client flags extracted from any position.
type clientFlagValues struct {
	host, token, config, profile string
	jsonOut                      bool
	timeout                      time.Duration
}

// extractClientFlags removes --host, --token, --config, --profile, --json and --timeout wherever they appear, leaving command flags and positionals in order.
func extractClientFlags(args []string) (clientFlagValues, []string, error) {
	values := clientFlagValues{timeout: defaultClientTimeout}

	rest := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			rest = append(rest, args[index+1:]...)
			break
		}

		key, value, hasValue := strings.Cut(arg, "=")
		if !strings.HasPrefix(key, "-") {
			rest = append(rest, arg)
			continue
		}

		switch strings.TrimLeft(key, "-") {
		case "host", "token", "config", "profile", "timeout":
			if !hasValue {
				index++
				if index >= len(args) {
					return clientFlagValues{}, nil, fmt.Errorf("%s needs an argument", key)
				}

				value = args[index]
			}

			switch strings.TrimLeft(key, "-") {
			case "host":
				values.host = value
			case "token":
				values.token = value
			case "config":
				values.config = value
			case "profile":
				values.profile = value
			case "timeout":
				parsed, err := time.ParseDuration(value)
				if err != nil {
					return clientFlagValues{}, nil, fmt.Errorf("--timeout: %w", err)
				}

				values.timeout = parsed
			}
		case "json":
			if !hasValue {
				values.jsonOut = true
				continue
			}

			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return clientFlagValues{}, nil, fmt.Errorf("--json: %w", err)
			}

			values.jsonOut = parsed
		default:
			rest = append(rest, arg)
		}
	}

	return values, rest, nil
}

// clientFlags loads global flags from any position and falls back to WT_DEPLOY_HOST, WT_TOKEN and the selected profile.
func clientFlags(args []string, requireHost bool) (clientOptions, []string, error) {
	values, rest, err := extractClientFlags(args)
	if err != nil {
		return clientOptions{}, nil, err
	}

	fileCfg, err := loadFileConfig(values.config)
	if err != nil {
		return clientOptions{}, nil, err
	}

	profileName := activeProfileName(values.profile, fileCfg)

	profile, err := resolveProfile(fileCfg, profileName)
	if err != nil {
		return clientOptions{}, nil, err
	}

	host := firstNonEmpty(values.host, os.Getenv("WT_DEPLOY_HOST"), profile.Host)
	token := firstNonEmpty(values.token, os.Getenv("WT_TOKEN"), profile.Token)

	if requireHost && strings.TrimSpace(host) == "" {
		return clientOptions{}, nil, errors.New("set --host, WT_DEPLOY_HOST, or host in the config file")
	}

	return clientOptions{
		host:      host,
		token:     token,
		profile:   profileName,
		appDomain: profile.AppDomain,
		jsonOut:   values.jsonOut,
		timeout:   values.timeout,
	}, rest, nil
}

func supervisorURL(host, path string) string {
	return baseURL(host) + path
}

// apiDo performs one API request with the client timeout and returns at most 8 MiB of the response body.
func apiDo(cfg clientOptions, method, path string, body io.Reader, contentType string) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, supervisorURL(cfg.host, path), body)
	if err != nil {
		return 0, nil, nil, err
	}

	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	client := &http.Client{Timeout: cfg.timeout}

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, largeResponseLimit))
	if err != nil {
		return resp.StatusCode, resp.Header, nil, err
	}

	return resp.StatusCode, resp.Header, data, nil
}

// checkAPI turns a non-2xx API response into an error that includes the response body.
func checkAPI(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}

	text := strings.TrimSpace(string(body))
	if strings.HasPrefix(text, "<") {
		if title := htmlErrorTitle(text); title != "" {
			return fmt.Errorf("status %d: %s", status, title)
		}

		return fmt.Errorf("status %d: non-JSON error page (%d bytes)", status, len(body))
	}

	return fmt.Errorf("status %d: %s", status, text)
}

// htmlErrorTitle pulls the title out of an HTML error page, so an outage reads as one line instead of a markup dump.
func htmlErrorTitle(page string) string {
	lower := strings.ToLower(page)
	start := strings.Index(lower, "<title>")

	if start < 0 {
		return ""
	}

	title := strings.TrimSpace(page[start+len("<title>"):])
	if end := strings.Index(strings.ToLower(title), "</title>"); end >= 0 {
		title = strings.TrimSpace(title[:end])
	}

	return title
}

func printRawOrJSON(cfg clientOptions, body []byte) {
	if cfg.jsonOut {
		fmt.Println(strings.TrimSpace(string(body)))
		return
	}

	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		fmt.Println(strings.TrimSpace(string(body)))
		return
	}

	out, _ := json.Marshal(value)
	fmt.Println(string(out))
}
