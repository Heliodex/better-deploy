package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const maxSecretBytes = 64 << 10

type secretKeysResponse struct {
	Keys []string `json:"keys"`
}

type secretMutationResponse struct {
	Key             string `json:"key"`
	RestartRequired bool   `json:"restart_required"`
}

// validSecretKey matches the supervisor's environment-style key rules.
func validSecretKey(key string) bool {
	if key == "" || len(key) > 128 {
		return false
	}

	for i, char := range key {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char == '_':
		case char >= '0' && char <= '9' && i > 0:
		default:
			return false
		}
	}

	return true
}

// readSecretValue reads a secret from a file or stdin, bounded to match the supervisor.
func readSecretValue(file string) (string, error) {
	var (
		data []byte
		err  error
	)

	if file != "" {
		data, err = os.ReadFile(file)
	} else {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, maxSecretBytes+1))
	}

	if err != nil {
		return "", err
	}

	if len(data) > maxSecretBytes {
		return "", fmt.Errorf("secret value exceeds %d bytes", maxSecretBytes)
	}

	value := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if strings.ContainsRune(value, '\x00') {
		return "", errors.New("secret value contains a NUL byte")
	}

	return value, nil
}

// runSecretsList prints an app's secret key names. Values are never returned by the supervisor.
func runSecretsList(cfg clientOptions, name string, args []string) error {
	flags := newFlagSet("secrets list")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	status, _, body, err := apiDo(cfg, http.MethodGet, "/apps/"+name+"/secrets", nil, "")
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

	var result secretKeysResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}

	if len(result.Keys) == 0 {
		fmt.Println("no secrets")
		return nil
	}

	for _, key := range result.Keys {
		fmt.Println(key)
	}

	return nil
}

// runSecretsSet reads a value from stdin or --file and writes one secret.
func runSecretsSet(cfg clientOptions, name string, args []string) error {
	var (
		file string
		key  string
	)

	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--file" && index+1 < len(args):
			index++
			file = args[index]
		case strings.HasPrefix(arg, "--file="):
			file = strings.TrimPrefix(arg, "--file=")
		case strings.HasPrefix(arg, "-"):
			return fmt.Errorf("unexpected flag %q", arg)
		default:
			if key != "" {
				return fmt.Errorf("unexpected argument %q", arg)
			}

			key = arg
		}
	}

	if key == "" {
		return errors.New("usage: wt secrets <app> set <KEY> [--file PATH], with the value on stdin")
	}

	if !validSecretKey(key) {
		return fmt.Errorf("invalid secret key %q", key)
	}

	value, err := readSecretValue(file)
	if err != nil {
		return fmt.Errorf("read secret value: %w", err)
	}

	payload, err := json.Marshal(map[string]string{"value": value})
	if err != nil {
		return err
	}

	status, _, body, err := apiDo(cfg, http.MethodPut, "/apps/"+name+"/secrets/"+url.PathEscape(key), strings.NewReader(string(payload)), "application/json")
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

	var result secretMutationResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}

	fmt.Printf("set %s", result.Key)

	if result.RestartRequired {
		fmt.Print(" (restart required)")
	}

	fmt.Println()

	return nil
}

// runSecretsDelete removes one secret.
func runSecretsDelete(cfg clientOptions, name string, args []string) error {
	flags := newFlagSet("secrets delete")
	if err := flags.Parse(args); err != nil {
		return err
	}

	if flags.NArg() != 1 {
		return errors.New("usage: wt secrets <app> delete <KEY>")
	}

	key := flags.Arg(0)
	if !validSecretKey(key) {
		return fmt.Errorf("invalid secret key %q", key)
	}

	status, _, body, err := apiDo(cfg, http.MethodDelete, "/apps/"+name+"/secrets/"+url.PathEscape(key), nil, "")
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

	var result secretMutationResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}

	fmt.Printf("deleted %s", result.Key)

	if result.RestartRequired {
		fmt.Print(" (restart required)")
	}

	fmt.Println()

	return nil
}

// runSecrets dispatches wt secrets <app> <list|set|delete>.
func runSecrets(args []string) error {
	cfg, rest, err := clientFlags(args, true)
	if err != nil {
		return err
	}

	if len(rest) < 2 {
		return errors.New("usage: wt secrets <app> <list|set|delete>")
	}

	name, action := rest[0], rest[1]
	if !validName(name) {
		return fmt.Errorf("invalid app name %q", name)
	}

	switch action {
	case "list":
		return runSecretsList(cfg, name, rest[2:])
	case "set":
		return runSecretsSet(cfg, name, rest[2:])
	case "delete":
		return runSecretsDelete(cfg, name, rest[2:])
	default:
		return fmt.Errorf("unknown secrets action %q", action)
	}
}
