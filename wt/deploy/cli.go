package main

import (
	"bufio"
	"encoding/json/v2"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultClientTimeout = 30 * time.Second

// fileConfig is the optional ~/.config/wt/config.json shared by CLI commands.
type fileConfig struct {
	Host     string `json:"host,omitempty"`
	Token    string `json:"token,omitempty"`
	Wasmtime string `json:"wasmtime,omitempty"`
	Target   string `json:"target,omitempty"`
	AppHost  string `json:"app_host,omitempty"`
}

type clientOptions struct {
	host, token string
	jsonOut     bool
	timeout     time.Duration
}

// configArg finds --config before flag parsing so file defaults can seed the flag values.
func configArg(args []string) string {
	for index, arg := range args {
		if arg == "--config" && index+1 < len(args) {
			return args[index+1]
		}
		if value, ok := strings.CutPrefix(arg, "--config="); ok {
			return value
		}
	}
	return ""
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
	return cfg, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// clientFlags loads flags and falls back to WT_DEPLOY_HOST, WT_TOKEN and the config file for missing values.
func clientFlags(name string, args []string, requireHost bool) (clientOptions, []string, error) {
	configPath := configArg(args)
	fileCfg, err := loadFileConfig(configPath)
	if err != nil {
		return clientOptions{}, nil, err
	}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	host := flags.String("host", firstNonEmpty(os.Getenv("WT_DEPLOY_HOST"), fileCfg.Host), "supervisor URL")
	token := flags.String("token", firstNonEmpty(os.Getenv("WT_TOKEN"), fileCfg.Token), "bearer token")
	config := flags.String("config", configPath, "config file (default ~/.config/wt/config.json)")
	jsonOut := flags.Bool("json", false, "print raw JSON")
	timeout := flags.Duration("timeout", defaultClientTimeout, "HTTP timeout")
	if err := flags.Parse(args); err != nil {
		return clientOptions{}, nil, err
	}
	if *config != configPath {
		fileCfg, err = loadFileConfig(*config)
		if err != nil {
			return clientOptions{}, nil, err
		}
		if *host == "" {
			*host = fileCfg.Host
		}
		if *token == "" {
			*token = fileCfg.Token
		}
	}
	if requireHost && strings.TrimSpace(*host) == "" {
		return clientOptions{}, nil, fmt.Errorf("set --host, WT_DEPLOY_HOST or host in the config file")
	}
	return clientOptions{host: *host, token: *token, jsonOut: *jsonOut, timeout: *timeout}, flags.Args(), nil
}

func supervisorURL(host, path string) string {
	return baseURL(host) + path
}

// apiDo performs one API request with the client timeout and returns at most 8 MiB of the response body.
func apiDo(cfg clientOptions, method, path string, body io.Reader, contentType string) (int, http.Header, []byte, error) {
	req, err := http.NewRequest(method, supervisorURL(cfg.host, path), body)
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
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
	return fmt.Errorf("status %d: %s", status, strings.TrimSpace(string(body)))
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

func appNameArg(args []string, command string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("usage: wt %s <app>", command)
	}
	return args[0], nil
}

func runApps(args []string) error {
	cfg, rest, err := clientFlags("apps", args, true)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q", rest[0])
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
		if len(digest) > 12 {
			digest = digest[:12]
		}
		fmt.Printf("%-24s %-32s %-10s %-6v %-7d %s\n", app.Name, app.Host, app.Status, app.Ready, app.PID, digest)
	}
	return nil
}

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
	req, err := http.NewRequest(http.MethodGet, supervisorURL(cfg.host, path), nil)
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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
	opts := logOptions{tail: 200}
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
				return "", opts, fmt.Errorf("tail must be an integer")
			}
		case strings.HasPrefix(arg, "--tail="):
			opts.tail, err = strconv.Atoi(strings.TrimPrefix(arg, "--tail="))
			if err != nil {
				return "", opts, fmt.Errorf("tail must be an integer")
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
	cfg, rest, err := clientFlags("logs", args, true)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: wt logs <app> [--tail N] [--level LEVEL] [--source app|supervisor] [--q TEXT] [--follow] [--json]")
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

func runRollback(args []string) error {
	cfg, rest, err := clientFlags("rollback", args, true)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return fmt.Errorf("usage: wt rollback <app> <deployment-id> [--backup SNAPSHOT]")
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
