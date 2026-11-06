package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// checkContract regenerates the contract in a temporary directory and compares it with the checked-in files.
func checkContract(dir, web, appName string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	generated, err := os.MkdirTemp(abs, ".wt-contract-check-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(generated)

	if err := generateContractTo(abs, web, appName, generated, true); err != nil {
		return err
	}

	if err := compareContractTrees(generated, filepath.Join(abs, "contract")); err != nil {
		return err
	}

	return checkClient(abs, web, generated)
}

// checkClient compares the generated fetch client with the checked-in one when the app keeps an endpoint registry.
func checkClient(dir, web, generatedDir string) error {
	_, registryErr := os.Stat(filepath.Join(dir, "contract", "endpoints.ts"))
	current := filepath.Join(dir, web, "src", "lib", "api.gen.ts")

	if os.IsNotExist(registryErr) {
		if _, err := os.Stat(current); err == nil {
			return errors.New("contract: src/lib/api.gen.ts is stale (no endpoint registry)")
		}

		return nil
	}

	if registryErr != nil {
		return registryErr
	}

	return compareContractFile(
		filepath.Join(generatedDir, "api.gen.ts"),
		current,
		"src/lib/api.gen.ts",
	)
}

// compareContractTrees reports every generated file that is missing, stale or different in currentDir.
func compareContractTrees(generatedDir, currentDir string) error {
	problems := []string{}

	if err := compareContractFile(
		filepath.Join(generatedDir, "contract.gen.go"),
		filepath.Join(currentDir, "contract.gen.go"),
		"contract/contract.gen.go",
	); err != nil {
		problems = append(problems, err.Error())
	}

	generated, err := contractSchemaFiles(generatedDir)
	if err != nil {
		return err
	}

	current, err := contractSchemaFiles(currentDir)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(generated)+len(current))
	for name := range generated {
		names = append(names, name)
	}

	for name := range current {
		if _, ok := generated[name]; !ok {
			names = append(names, name)
		}
	}

	sort.Strings(names)

	for _, name := range names {
		generatedPath, generatedOK := generated[name]
		currentPath, currentOK := current[name]
		label := "contract/schemas/" + name

		switch {
		case !generatedOK:
			problems = append(problems, label+" is stale")
		case !currentOK:
			problems = append(problems, label+" is missing")
		default:
			generatedData, err := os.ReadFile(generatedPath)
			if err != nil {
				return err
			}

			currentData, err := os.ReadFile(currentPath)
			if err != nil {
				return err
			}

			if !bytes.Equal(generatedData, currentData) {
				problems = append(problems, label+" differs")
			}
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("contract is out of date:\n  - %s\nrun `wt contract --dir .` to regenerate", strings.Join(problems, "\n  - "))
	}

	return nil
}

// compareContractFile reports one generated file as missing or different.
func compareContractFile(generatedPath, currentPath, label string) error {
	generated, err := os.ReadFile(generatedPath)
	if err != nil {
		return err
	}

	current, err := os.ReadFile(currentPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("%s is missing", label)
	}

	if err != nil {
		return err
	}

	if !bytes.Equal(generated, current) {
		return fmt.Errorf("%s differs", label)
	}

	return nil
}

// contractSchemaFiles maps schema file names to paths under one contract directory.
func contractSchemaFiles(dir string) (map[string]string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "schemas", "*.json"))
	if err != nil {
		return nil, err
	}

	files := make(map[string]string, len(paths))
	for _, path := range paths {
		files[filepath.Base(path)] = path
	}

	return files, nil
}
