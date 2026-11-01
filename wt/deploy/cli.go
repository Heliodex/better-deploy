package main

import (
	"bufio"
	"bytes"
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

func apiJSON(cfg clientOptions, method, path string, value any) (int, http.Header, []byte, error) {
	var body io.Reader
	if value != nil {
		data, err := json.Marshal(value)
		if err != nil {
			return 0, nil, nil, err
		}
		body = bytes.NewReader(data)
	}
	return apiDo(cfg, method, path, body, "application/json")
}

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

func runLogs(args []string) error {
	cfg, rest, err := clientFlags("logs", args, true)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: wt logs <app> [--tail N] [--level LEVEL] [--source app|supervisor] [--q TEXT] [--follow] [--json]")
	}
	name := rest[0]
	tailValue, levelValue, sourceValue, queryValue := 200, "", "", ""
	followValue := false
	for index := 1; index < len(rest); index++ {
		arg := rest[index]
		switch {
		case arg == "--follow":
			followValue = true
		case arg == "--json":
			cfg.jsonOut = true
		case arg == "--tail" && index+1 < len(rest):
			index++
			tailValue, err = strconv.Atoi(rest[index])
			if err != nil {
				return fmt.Errorf("tail must be an integer")
			}
		case strings.HasPrefix(arg, "--tail="):
			tailValue, err = strconv.Atoi(strings.TrimPrefix(arg, "--tail="))
			if err != nil {
				return fmt.Errorf("tail must be an integer")
			}
		case arg == "--level" && index+1 < len(rest):
			index++
			levelValue = rest[index]
		case strings.HasPrefix(arg, "--level="):
			levelValue = strings.TrimPrefix(arg, "--level=")
		case arg == "--source" && index+1 < len(rest):
			index++
			sourceValue = rest[index]
		case strings.HasPrefix(arg, "--source="):
			sourceValue = strings.TrimPrefix(arg, "--source=")
		case arg == "--q" && index+1 < len(rest):
			index++
			queryValue = rest[index]
		case strings.HasPrefix(arg, "--q="):
			queryValue = strings.TrimPrefix(arg, "--q=")
		default:
			return fmt.Errorf("unexpected argument %q", arg)
		}
	}
	values := map[string]string{"tail": strconv.Itoa(tailValue)}
	if levelValue != "" {
		values["level"] = levelValue
	}
	if sourceValue != "" {
		values["source"] = sourceValue
	}
	if queryValue != "" {
		values["q"] = queryValue
	}
	path := "/apps/" + name + "/logs"
	if followValue {
		path += "/stream"
	}
	separator := "?"
	for key, value := range values {
		path += separator + key + "=" + value
		separator = "&"
	}
	if followValue {
		return followLogs(cfg, path)
	}
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

func derefLog(entry *logLine) []logLine {
	if entry == nil {
		return nil
	}
	return []logLine{*entry}
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
		if strings.HasPrefix(rest[index], "--backup=") {
			backup = strings.TrimPrefix(rest[index], "--backup=")
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
		status, _, body, err := apiDo(cfg, http.MethodGet, "/apps/"+name+"/backups", nil, "")
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
	case "create":
		status, _, body, err := apiDo(cfg, http.MethodPost, "/apps/"+name+"/backups", nil, "")
		if err != nil {
			return err
		}
		if err := checkAPI(status, body); err != nil {
			return err
		}
		printRawOrJSON(cfg, body)
		return nil
	case "restore":
		if len(rest) < 2 {
			return fmt.Errorf("usage: wt backup restore <app> <snapshot>")
		}
		status, _, body, err := apiDo(cfg, http.MethodPost, "/apps/"+name+"/backups/"+rest[1]+"/restore", nil, "")
		if err != nil {
			return err
		}
		if err := checkAPI(status, body); err != nil {
			return err
		}
		printRawOrJSON(cfg, body)
		return nil
	case "download":
		if len(rest) < 2 {
			return fmt.Errorf("usage: wt backup download <app> <snapshot> [--out FILE]")
		}
		out := rest[1] + ".tar.gz"
		for index := 2; index < len(rest); index++ {
			if rest[index] == "--out" && index+1 < len(rest) {
				index++
				out = rest[index]
			} else if strings.HasPrefix(rest[index], "--out=") {
				out = strings.TrimPrefix(rest[index], "--out=")
			}
		}
		if err := backupDownload(cfg, name, rest[1], out); err != nil {
			return err
		}
		fmt.Println("wrote", out)
		return nil
	case "import":
		if len(rest) < 2 {
			return fmt.Errorf("usage: wt backup import <app> <archive.tar.gz>")
		}
		return backupImport(cfg, name, rest[1])
	default:
		return fmt.Errorf("unknown backup action %q", action)
	}
}

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
	case "set":
		data := ""
		file := ""
		ifMatch := ""
		for index := 1; index < len(rest); index++ {
			switch {
			case rest[index] == "--data" && index+1 < len(rest):
				index++
				data = rest[index]
			case strings.HasPrefix(rest[index], "--data="):
				data = strings.TrimPrefix(rest[index], "--data=")
			case rest[index] == "--file" && index+1 < len(rest):
				index++
				file = rest[index]
			case strings.HasPrefix(rest[index], "--file="):
				file = strings.TrimPrefix(rest[index], "--file=")
			case rest[index] == "--if-match" && index+1 < len(rest):
				index++
				ifMatch = rest[index]
			case strings.HasPrefix(rest[index], "--if-match="):
				ifMatch = strings.TrimPrefix(rest[index], "--if-match=")
			default:
				return fmt.Errorf("unexpected argument %q", rest[index])
			}
		}
		if data == "" && file != "" {
			raw, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			data = string(raw)
		}
		if data == "" {
			return fmt.Errorf("provide --data JSON or --file patch.json")
		}
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
	default:
		return fmt.Errorf("unknown config action %q", action)
	}
}

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

func commandNames() []string {
	return []string{"apps", "logs", "deploy", "build", "bundle", "dev", "new", "rollback", "backup", "config", "open", "completion", "help"}
}

func commandHelp(command string) {
	switch command {
	case "apps":
		fmt.Println("usage: wt apps [--host URL] [--token TOKEN] [--config FILE] [--json]")
	case "logs":
		fmt.Println("usage: wt logs <app> [--tail N] [--level LEVEL] [--source app|supervisor] [--q TEXT] [--follow] [--json]")
	case "deploy":
		fmt.Println("usage: wt deploy [--dir DIR] [--host URL] [--token TOKEN] [--app-host HOST] [--json]")
	case "rollback":
		fmt.Println("usage: wt rollback <app> <deployment-id> [--backup SNAPSHOT] [--host URL] [--token TOKEN] [--json]")
	case "backup":
		fmt.Println("usage: wt backup <list|create|restore|download|import> <app> ...")
	case "config":
		fmt.Println("usage: wt config show <app> [--json] | wt config set <app> --data JSON|--file FILE [--if-match ETAG]")
	case "open":
		fmt.Println("usage: wt open <app> [--print] [--host URL] [--token TOKEN]")
	case "build", "bundle":
		fmt.Println("usage: wt build --dir DIR --out app.tar.gz [--wasmtime PATH] [--skip-build] [--skip-compile]")
	case "completion":
		fmt.Println("usage: wt completion <bash|zsh|fish>")
	default:
		fmt.Printf("usage: wt <%s>\n", strings.Join(commandNames(), "|"))
	}
}
