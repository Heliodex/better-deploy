package main

import (
	"bufio"
	"encoding/json/v2"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// backupJSON performs one backup API call and returns the body.
func backupJSON(cfg clientOptions, method, path string) ([]byte, error) {
	status, _, body, err := apiDo(cfg, method, path, nil, "")
	if err != nil {
		return nil, err
	}
	if err := checkAPI(status, body); err != nil {
		return nil, err
	}
	return body, nil
}

// backupOutPath reads the optional --out argument, defaulting to <snapshot>.tar.gz.
func backupOutPath(rest []string) string {
	out := rest[1] + ".tar.gz"
	for index := 2; index < len(rest); index++ {
		if rest[index] == "--out" && index+1 < len(rest) {
			index++
			out = rest[index]
		} else if value, ok := strings.CutPrefix(rest[index], "--out="); ok {
			out = value
		}
	}
	return out
}

// backupImport streams the archive as a multipart upload so large snapshots are not held in memory.
func backupImport(cfg clientOptions, name, archive string) error {
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	go func() {
		file, err := os.Open(archive)
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		defer file.Close()
		part, err := multipartWriter.CreateFormFile("snapshot", filepath.Base(archive))
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, file); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_ = writer.CloseWithError(multipartWriter.Close())
	}()
	req, err := http.NewRequest(http.MethodPost, supervisorURL(cfg.host, "/apps/"+name+"/backups/import"), reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	resp, err := (&http.Client{Timeout: 0}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := checkAPI(resp.StatusCode, body); err != nil {
		return err
	}
	printRawOrJSON(cfg, body)
	return nil
}

// backupDownload streams a snapshot archive to out without the client timeout, since snapshots can be large.
func backupDownload(cfg clientOptions, name, snapshot, out string) error {
	req, err := http.NewRequest(http.MethodGet, supervisorURL(cfg.host, "/apps/"+name+"/backups/"+snapshot+"/download"), nil)
	if err != nil {
		return err
	}
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	resp, err := (&http.Client{Timeout: 0}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	file, err := os.Create(out)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, resp.Body)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// runBackupList prints the retained snapshots and marks the current one.
func runBackupList(cfg clientOptions, name string) error {
	body, err := backupJSON(cfg, http.MethodGet, "/apps/"+name+"/backups")
	if err != nil {
		return err
	}
	if cfg.jsonOut {
		printRawOrJSON(cfg, body)
		return nil
	}
	var result struct {
		Current string `json:"current"`
		Backups []struct {
			Name    string `json:"name"`
			Kind    string `json:"kind"`
			Size    int64  `json:"size"`
			HasData bool   `json:"hasData"`
		} `json:"backups"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	for _, backup := range result.Backups {
		marker := " "
		if backup.Name == result.Current {
			marker = "*"
		}
		fmt.Printf("%s %-42s %-10s %d\n", marker, backup.Name, backup.Kind, backup.Size)
	}
	return nil
}

// runBackupCreate takes a new snapshot.
func runBackupCreate(cfg clientOptions, name string) error {
	body, err := backupJSON(cfg, http.MethodPost, "/apps/"+name+"/backups")
	if err != nil {
		return err
	}
	printRawOrJSON(cfg, body)
	return nil
}

// runBackupRestore restores one snapshot.
func runBackupRestore(cfg clientOptions, name, snapshot string) error {
	body, err := backupJSON(cfg, http.MethodPost, "/apps/"+name+"/backups/"+snapshot+"/restore")
	if err != nil {
		return err
	}
	printRawOrJSON(cfg, body)
	return nil
}

// runBackupDownload saves one snapshot archive.
func runBackupDownload(cfg clientOptions, name, snapshot, out string) error {
	if err := backupDownload(cfg, name, snapshot, out); err != nil {
		return err
	}
	fmt.Println("wrote", out)
	return nil
}

func runBackup(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: wt backup <list|create|restore|download|import> <app> ...")
	}
	action := args[0]
	cfg, rest, err := clientFlags("backup "+action, args[1:], true)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: wt backup %s <app> ...", action)
	}
	name := rest[0]
	switch action {
	case "list":
		return runBackupList(cfg, name)
	case "create":
		return runBackupCreate(cfg, name)
	case "restore":
		if len(rest) < 2 {
			return fmt.Errorf("usage: wt backup restore <app> <snapshot>")
		}
		return runBackupRestore(cfg, name, rest[1])
	case "download":
		if len(rest) < 2 {
			return fmt.Errorf("usage: wt backup download <app> <snapshot> [--out FILE]")
		}
		return runBackupDownload(cfg, name, rest[1], backupOutPath(rest))
	case "import":
		if len(rest) < 2 {
			return fmt.Errorf("usage: wt backup import <app> <archive.tar.gz>")
		}
		return backupImport(cfg, name, rest[1])
	default:
		return fmt.Errorf("unknown backup action %q", action)
	}
}

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
	req, err := http.NewRequest(http.MethodPatch, supervisorURL(cfg.host, "/apps/"+name+"/config"), strings.NewReader(data))
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
		return fmt.Errorf("provide --data JSON or --file patch.json")
	}
	return patchConfig(cfg, name, data, opts.ifMatch)
}

func runConfig(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: wt config <show|set> <app>")
	}
	action := args[0]
	cfg, rest, err := clientFlags("config "+action, args[1:], true)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: wt config %s <app>", action)
	}
	name := rest[0]
	switch action {
	case "show":
		return runConfigShow(cfg, name)
	case "set":
		return runConfigSet(cfg, name, rest)
	default:
		return fmt.Errorf("unknown config action %q", action)
	}
}

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

// runCompletion prints a completion script for the selected shell.
func runCompletion(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: wt completion <bash|zsh|fish>")
	}
	commands := []string{"apps", "logs", "deploy", "bundle", "dev", "new", "rollback", "backup", "config", "open", "completion", "help"}
	switch args[0] {
	case "bash":
		fmt.Printf("_wt() {\n  local cur\n  cur=\"${COMP_WORDS[COMP_CWORD]}\"\n  COMPREPLY=( $(compgen -W %q -- \"$cur\") )\n}\ncomplete -F _wt wt-deploy\n", strings.Join(commands, " "))
	case "zsh":
		fmt.Printf("#compdef wt-deploy\n_arguments '1:command:(%s)'\n", strings.Join(commands, " "))
	case "fish":
		for _, command := range commands {
			fmt.Printf("complete -c wt-deploy -f -n '__fish_use_subcommand' -a %s\n", command)
		}
	default:
		return fmt.Errorf("unknown shell %q", args[0])
	}
	return nil
}
