package main

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

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
	cfg, rest, err := clientFlags(args, true)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return fmt.Errorf("usage: wt backup <list|create|restore|download|import> <app>")
	}
	action, name := rest[0], rest[1]
	switch action {
	case "list":
		return runBackupList(cfg, name)
	case "create":
		return runBackupCreate(cfg, name)
	case "restore":
		if len(rest) < 3 {
			return fmt.Errorf("usage: wt backup restore <app> <snapshot>")
		}
		return runBackupRestore(cfg, name, rest[2])
	case "download":
		if len(rest) < 3 {
			return fmt.Errorf("usage: wt backup download <app> <snapshot> [--out FILE]")
		}
		return runBackupDownload(cfg, name, rest[2], backupOutPath(rest[1:]))
	case "import":
		if len(rest) < 3 {
			return fmt.Errorf("usage: wt backup import <app> <archive.tar.gz>")
		}
		return backupImport(cfg, name, rest[2])
	default:
		return fmt.Errorf("unknown backup action %q", action)
	}
}
