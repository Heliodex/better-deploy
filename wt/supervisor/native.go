package main

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ensureNativeArtifact checks the uploaded native binary and makes it executable.
// The namespace root has no dynamic loader, so a cgo or otherwise dynamically linked binary cannot start there.
func ensureNativeArtifact(dir string) error {
	path := filepath.Join(dir, "main")

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("native app: read main: %w", err)
	}

	if !info.Mode().IsRegular() {
		return errors.New("native app: main is not a regular file")
	}

	if info.Mode().Perm()&0o111 == 0 {
		if err := os.Chmod(path, 0o755); err != nil {
			return fmt.Errorf("native app: make main executable: %w", err)
		}
	}

	file, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("native app: main is not an ELF binary: %w", err)
	}

	defer file.Close()

	for _, prog := range file.Progs {
		if prog.Type == elf.PT_INTERP {
			return errors.New("native app: main must be statically linked (build with CGO_ENABLED=0)")
		}
	}

	return nil
}

// appSource returns the process path and epoch setting for one launch attempt.
func (s *supervisor) appSource(dir string, cfg appConfig) (source string, epoch bool, err error) {
	if cfg.Runtime == runtimeNative {
		if err := ensureNativeArtifact(dir); err != nil {
			return "", false, err
		}

		return filepath.Join(dir, "main"), false, nil
	}

	epoch, err = s.ensureArtifact(dir, cfg.requestTimeout > 0)
	if err != nil {
		return "", false, err
	}

	return filepath.Join(dir, "main.cwasm"), epoch, nil
}
