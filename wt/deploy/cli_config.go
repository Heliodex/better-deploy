package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// configSetOptions are the flags accepted by wt config set.
type configSetOptions struct {
	data, file, ifMatch string
}

func parseConfigSetOptions(rest []string) (configSetOptions, error) {
	var opts configSetOptions

	for index := 1; index < len(rest); index++ {
		switch {
		case rest[index] == "--data" && index+1 < len(rest):
			index++
			opts.data = rest[index]
		case strings.HasPrefix(rest[index], "--data="):
			opts.data = strings.TrimPrefix(rest[index], "--data=")
		case rest[index] == "--file" && index+1 < len(rest):
			index++
			opts.file = rest[index]
		case strings.HasPrefix(rest[index], "--file="):
			opts.file = strings.TrimPrefix(rest[index], "--file=")
		case rest[index] == "--if-match" && index+1 < len(rest):
			index++
			opts.ifMatch = rest[index]
		case strings.HasPrefix(rest[index], "--if-match="):
			opts.ifMatch = strings.TrimPrefix(rest[index], "--if-match=")
		default:
			return opts, fmt.Errorf("unexpected argument %q", rest[index])
		}
	}

	return opts, nil
}

// configSetData resolves a --file patch into the request body.
func configSetData(opts configSetOptions) (string, error) {
	if opts.data != "" || opts.file == "" {
		return opts.data, nil
	}

	raw, err := os.ReadFile(opts.file)
	if err != nil {
		return "", err
	}

	return string(raw), nil
}

// patchConfig sends one config patch and prints the result.
func patchConfig(cfg clientOptions, name, data, ifMatch string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPatch, supervisorURL(cfg.host, "/apps/"+name+"/config"), strings.NewReader(data))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}

	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}

	resp, err := (&http.Client{Timeout: cfg.timeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if err := checkAPI(resp.StatusCode, body); err != nil {
		return err
	}

	printRawOrJSON(cfg, body)

	return nil
}

// runConfigShow prints one app config and its ETag.
func runConfigShow(cfg clientOptions, name string) error {
	status, header, body, err := apiDo(cfg, http.MethodGet, "/apps/"+name+"/config", nil, "")
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

	if etag := header.Get("ETag"); etag != "" {
		fmt.Fprintln(os.Stderr, "ETag:", etag)
	}

	printRawOrJSON(cfg, body)

	return nil
}

// runConfigSet applies one patch from --data or --file.
func runConfigSet(cfg clientOptions, name string, rest []string) error {
	opts, err := parseConfigSetOptions(rest)
	if err != nil {
		return err
	}

	data, err := configSetData(opts)
	if err != nil {
		return err
	}

	if data == "" {
		return errors.New("provide --data JSON or --file patch.json")
	}

	return patchConfig(cfg, name, data, opts.ifMatch)
}

func runConfig(args []string) error {
	cfg, rest, err := clientFlags(args, true)
	if err != nil {
		return err
	}

	if len(rest) < 2 {
		return errors.New("usage: wt config <show|set> <app>")
	}

	action, name := rest[0], rest[1]
	switch action {
	case "show":
		return runConfigShow(cfg, name)
	case "set":
		return runConfigSet(cfg, name, rest[1:])
	default:
		return fmt.Errorf("unknown config action %q", action)
	}
}
