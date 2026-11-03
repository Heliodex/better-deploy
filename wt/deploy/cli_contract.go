package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// runContract regenerates the app's JSON Schema and Go contract from web/src/lib/contract.ts.
func runContract(args []string) error {
	flags := flag.NewFlagSet("contract", flag.ContinueOnError)

	dir := flags.String("dir", ".", "app directory containing app.json")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	generator := filepath.Join(*dir, "contract", "generate.go")
	if _, err := os.Stat(generator); err != nil {
		if os.IsNotExist(err) {
			return errors.New("app has no contract generator; scaffold with --contract=valibot or use hand-written types")
		}

		return err
	}

	cmd := exec.CommandContext(context.Background(), "go", "generate", "./contract/")
	cmd.Dir = *dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("contract generation: %w", err)
	}

	fmt.Println("contract: generated")

	return nil
}
