package main

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type logLine struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Source  string `json:"source"`
	Message string `json:"message"`
}

func derefLog(entry *logLine) []logLine {
	if entry == nil {
		return nil
	}

	return []logLine{*entry}
}

// followLogs consumes the supervisor's SSE log stream and prints each decoded entry.
func followLogs(cfg clientOptions, path string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, supervisorURL(cfg.host, path), nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+cfg.token)

	resp, err := (&http.Client{Timeout: 0}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, smallResponseLimit))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var message struct {
			Type string    `json:"type"`
			Logs []logLine `json:"logs"`
			Log  *logLine  `json:"log"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &message); err != nil {
			continue
		}

		for _, entry := range append(message.Logs, derefLog(message.Log)...) {
			if cfg.jsonOut {
				fmt.Printf("{\"time\":%q,\"level\":%q,\"source\":%q,\"message\":%q}\n", entry.Time, entry.Level, entry.Source, entry.Message)
				continue
			}

			fmt.Printf("%s %-6s %-10s %s\n", entry.Time, entry.Level, entry.Source, entry.Message)
		}
	}

	return scanner.Err()
}

// printLogs fetches one page of logs and prints the table or raw JSON.
func printLogs(cfg clientOptions, path string) error {
	status, _, body, err := apiDo(cfg, http.MethodGet, path, nil, "")
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

	var result struct {
		Logs []logLine `json:"logs"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}

	for _, entry := range result.Logs {
		fmt.Printf("%s %-6s %-10s %s\n", entry.Time, entry.Level, entry.Source, entry.Message)
	}

	return nil
}

// logOptions are the filters accepted by wt logs.
type logOptions struct {
	level, source, query string
	tail                 int
	follow               bool
}

// parseLogOptions parses wt logs filters and defaults to the last 200 entries.
func parseLogOptions(cfg *clientOptions, rest []string) (string, logOptions, error) {
	name := rest[0]
	opts := logOptions{tail: defaultLogTail}

	var err error

	for index := 1; index < len(rest); index++ {
		arg := rest[index]
		switch {
		case arg == "--follow":
			opts.follow = true
		case arg == "--json":
			cfg.jsonOut = true
		case arg == "--tail" && index+1 < len(rest):
			index++

			opts.tail, err = strconv.Atoi(rest[index])
			if err != nil {
				return "", opts, errors.New("tail must be an integer")
			}
		case strings.HasPrefix(arg, "--tail="):
			opts.tail, err = strconv.Atoi(strings.TrimPrefix(arg, "--tail="))
			if err != nil {
				return "", opts, errors.New("tail must be an integer")
			}
		case arg == "--level" && index+1 < len(rest):
			index++
			opts.level = rest[index]
		case strings.HasPrefix(arg, "--level="):
			opts.level = strings.TrimPrefix(arg, "--level=")
		case arg == "--source" && index+1 < len(rest):
			index++
			opts.source = rest[index]
		case strings.HasPrefix(arg, "--source="):
			opts.source = strings.TrimPrefix(arg, "--source=")
		case arg == "--q" && index+1 < len(rest):
			index++
			opts.query = rest[index]
		case strings.HasPrefix(arg, "--q="):
			opts.query = strings.TrimPrefix(arg, "--q=")
		default:
			return "", opts, fmt.Errorf("unexpected argument %q", arg)
		}
	}

	return name, opts, nil
}

// logPath builds /apps/<name>/logs with the requested filters.
func logPath(name string, opts logOptions) string {
	values := map[string]string{"tail": strconv.Itoa(opts.tail)}
	if opts.level != "" {
		values["level"] = opts.level
	}

	if opts.source != "" {
		values["source"] = opts.source
	}

	if opts.query != "" {
		values["q"] = opts.query
	}

	path := "/apps/" + name + "/logs"
	if opts.follow {
		path += "/stream"
	}

	var query strings.Builder

	separator := byte('?')
	for key, value := range values {
		query.WriteByte(separator)
		query.WriteString(key)
		query.WriteByte('=')
		query.WriteString(value)

		separator = '&'
	}

	return path + query.String()
}

func runLogs(args []string) error {
	cfg, rest, err := clientFlags(args, true)
	if err != nil {
		return err
	}

	if len(rest) == 0 {
		return errors.New("usage: wt logs <app> [--tail N] [--level LEVEL] [--source app|supervisor] [--q TEXT] [--follow] [--json]")
	}

	name, opts, err := parseLogOptions(&cfg, rest)
	if err != nil {
		return err
	}

	path := logPath(name, opts)
	if opts.follow {
		return followLogs(cfg, path)
	}

	return printLogs(cfg, path)
}
