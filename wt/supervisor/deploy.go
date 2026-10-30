package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func replaceDir(src, dst string) error {
	backup := ""
	if _, err := os.Stat(dst); err == nil {
		backup = filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".old-"+strconv.FormatInt(time.Now().UnixNano(), 36))
		if err := os.Rename(dst, backup); err != nil {
			return fmt.Errorf("move old app aside: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat app directory: %w", err)
	}

	if err := os.Rename(src, dst); err != nil {
		if backup != "" {
			_ = os.Rename(backup, dst)
		}
		return fmt.Errorf("install app: %w", err)
	}
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			log.Printf("remove old app %s: %v", backup, err)
		}
	}
	return nil
}

// archiveEntryPath cleans an archive entry and returns where it should be extracted, rejecting paths that escape dst.
func archiveEntryPath(dst, entry string) (name, target string, err error) {
	name = path.Clean(strings.TrimPrefix(entry, "./"))
	if name == "." || name == "" {
		return "", "", nil
	}
	if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
		return "", "", fmt.Errorf("archive entry %q escapes the app directory", entry)
	}
	target = filepath.Join(dst, filepath.FromSlash(name))
	rel, err := filepath.Rel(dst, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("archive entry %q escapes the app directory", entry)
	}
	return name, target, nil
}

func extractTarDir(target, name string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", name, err)
	}
	return nil
}

// extractTarReg writes one regular file, keeping the archive inside the size limit.
func extractTarReg(tr *tar.Reader, header *tar.Header, target, name string, total *int64) error {
	if header.Size < 0 || *total+header.Size > maxExtractBytes {
		return fmt.Errorf("archive is too large")
	}
	*total += header.Size
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create parent of %s: %w", name, err)
	}
	mode := os.FileMode(0o644)
	if header.FileInfo().Mode()&0o111 != 0 {
		mode = 0o755
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	_, copyErr := io.CopyN(out, tr, header.Size)
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("write %s: %w", name, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", name, closeErr)
	}
	return nil
}

func extractTarGz(src, dst string) error {
	file, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer file.Close()

	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("read gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var total int64
	files := 0

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		name, target, err := archiveEntryPath(dst, header.Name)
		if err != nil {
			return err
		}
		if name == "" {
			continue
		}
		files++
		if files > maxTarFiles {
			return fmt.Errorf("archive has too many entries")
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := extractTarDir(target, name); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := extractTarReg(tr, header, target, name, &total); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("archive entry %q is a link, which is not allowed", header.Name)
		}
	}
	return nil
}

// receiveBundle parses an uploaded bundle into a temporary directory and returns its app config.
func (s *supervisor) receiveBundle(w http.ResponseWriter, r *http.Request, name string) (dir string, cfg appConfig, cleanup func(), ok bool) {
	cleanup = func() {}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return "", appConfig{}, cleanup, false
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	file, _, err := r.FormFile("artifact")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing artifact field")
		return "", appConfig{}, cleanup, false
	}
	defer file.Close()

	archive, err := os.CreateTemp(s.cfg.appsDir, ".upload-*.tar.gz")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create temp file")
		return "", appConfig{}, cleanup, false
	}
	archiveName := archive.Name()
	cleanup = func() { _ = os.Remove(archiveName) }
	if _, err := io.Copy(archive, io.LimitReader(file, maxUploadBytes)); err != nil {
		_ = archive.Close()
		writeError(w, http.StatusBadRequest, "read artifact: "+err.Error())
		return "", appConfig{}, cleanup, false
	}
	if err := archive.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, "close temp file")
		return "", appConfig{}, cleanup, false
	}

	dir, err = os.MkdirTemp(s.cfg.appsDir, "."+name+".upload-")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create upload directory")
		return "", appConfig{}, cleanup, false
	}
	cleanup = func() {
		_ = os.Remove(archiveName)
		_ = os.RemoveAll(dir)
	}

	if err := extractTarGz(archiveName, dir); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", appConfig{}, cleanup, false
	}
	if _, err := os.Stat(filepath.Join(dir, "main.cwasm")); err != nil {
		writeError(w, http.StatusBadRequest, "archive must contain main.cwasm")
		return "", appConfig{}, cleanup, false
	}

	cfg, err = readAppConfig(dir, name, s.cfg.domain)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", appConfig{}, cleanup, false
	}
	cfg.Name = name
	if s.hostTaken(cfg.Host, name) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("host %q is already used by another app", cfg.Host))
		return "", appConfig{}, cleanup, false
	}
	return dir, cfg, cleanup, true
}

// drainApp waits for in-flight proxied requests to finish before an old version is stopped.
func drainApp(a *app, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		a.requests.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

type deployValidationResponse struct {
	Valid  bool              `json:"valid"`
	Config appConfigResponse `json:"config"`
}

func (s *supervisor) deploy(w http.ResponseWriter, r *http.Request, name string) {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()

	tmpDir, cfg, cleanup, ok := s.receiveBundle(w, r, name)
	defer cleanup()
	if !ok {
		return
	}

	if value := r.URL.Query().Get("dry_run"); value != "" {
		dryRun, err := strconv.ParseBool(value)
		if err != nil {
			writeError(w, http.StatusBadRequest, "dry_run must be a boolean")
			return
		}
		if dryRun {
			writeJSON(w, http.StatusOK, deployValidationResponse{Valid: true, Config: configResponse(cfg)})
			return
		}
	}

	s.mu.RLock()
	old := s.apps[name]
	s.mu.RUnlock()

	port, err := freePort()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "allocate port")
		return
	}
	next, err := s.spawn(name, tmpDir, cfg, port)
	if err != nil {
		log.Printf("deploy %s: %v", name, err)
		writeError(w, http.StatusBadGateway, "new version did not start: "+err.Error())
		return
	}

	finalDir := filepath.Join(s.cfg.appsDir, name)
	if err := replaceDir(tmpDir, finalDir); err != nil {
		s.stop(next)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	next.dir = finalDir

	s.register(next)
	if old != nil {
		if !drainApp(old, drainTimeout) {
			log.Printf("deploy %s: old version still handling requests after %s", name, drainTimeout)
		}
		s.stop(old)
	}

	log.Printf("deployed %s at %s (127.0.0.1:%d)", name, cfg.Host, port)
	writeJSON(w, http.StatusOK, s.info(next))
}
