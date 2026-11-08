package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type storageQueryResponse struct {
	Columns      []string `json:"columns"`
	Rows         [][]any  `json:"rows"`
	RowsAffected int64    `json:"rows_affected"`
	LastInsertID int64    `json:"last_insert_id"`
	Truncated    bool     `json:"truncated"`
}

// runDBQuery runs one read-only SQL statement on an app's remote database.
func runDBQuery(cfg clientOptions, name string, args []string) error {
	flags := newFlagSet("db query")
	sqlText := flags.String("sql", "", "SQL statement to run")
	paramsJSON := flags.String("params", "", "JSON array of bound parameters")
	maxRows := flags.Int("max-rows", 0, "maximum rows to return (server default applies when 0)")

	if err := flags.Parse(args); err != nil {
		return err
	}

	statement := strings.TrimSpace(*sqlText)
	if statement == "" && flags.NArg() > 0 {
		statement = strings.Join(flags.Args(), " ")
	}

	if statement == "" {
		return errors.New("usage: wt db <app> query <sql> [--params '[]'] [--max-rows N]")
	}

	payload := map[string]any{"sql": statement}

	if strings.TrimSpace(*paramsJSON) != "" {
		var params []any
		if err := json.Unmarshal([]byte(*paramsJSON), &params); err != nil {
			return fmt.Errorf("parse --params: %w", err)
		}

		payload["params"] = params
	}

	if *maxRows > 0 {
		payload["max_rows"] = *maxRows
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	status, _, body, err := apiDo(cfg, http.MethodPost, "/apps/"+name+"/database/query", strings.NewReader(string(data)), "application/json")
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

	var result storageQueryResponse
	if err := json.Unmarshal(body, &result); err != nil {
		printRawOrJSON(cfg, body)
		return nil
	}

	if len(result.Columns) == 0 && len(result.Rows) == 0 {
		fmt.Println("(no rows)")
		return nil
	}

	fmt.Println(strings.Join(result.Columns, "\t"))

	for _, row := range result.Rows {
		cells := make([]string, 0, len(row))
		for _, value := range row {
			cells = append(cells, formatDBValue(value))
		}

		fmt.Println(strings.Join(cells, "\t"))
	}

	if result.Truncated {
		fmt.Println("(truncated)")
	}

	return nil
}

// runDBExec runs one write SQL statement on an app's remote database.
func runDBExec(cfg clientOptions, name string, args []string) error {
	flags := newFlagSet("db exec")
	sqlText := flags.String("sql", "", "SQL statement to run")
	paramsJSON := flags.String("params", "", "JSON array of bound parameters")

	if err := flags.Parse(args); err != nil {
		return err
	}

	statement := strings.TrimSpace(*sqlText)
	if statement == "" && flags.NArg() > 0 {
		statement = strings.Join(flags.Args(), " ")
	}

	if statement == "" {
		return errors.New("usage: wt db <app> exec <sql> [--params '[]']")
	}

	payload := map[string]any{"sql": statement}

	if strings.TrimSpace(*paramsJSON) != "" {
		var params []any
		if err := json.Unmarshal([]byte(*paramsJSON), &params); err != nil {
			return fmt.Errorf("parse --params: %w", err)
		}

		payload["params"] = params
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	status, _, body, err := apiDo(cfg, http.MethodPost, "/apps/"+name+"/database/exec", strings.NewReader(string(data)), "application/json")
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

	var result storageQueryResponse
	if err := json.Unmarshal(body, &result); err != nil {
		printRawOrJSON(cfg, body)
		return nil
	}

	fmt.Printf("rows_affected=%d last_insert_id=%d\n", result.RowsAffected, result.LastInsertID)

	return nil
}

func formatDBValue(value any) string {
	if value == nil {
		return "NULL"
	}

	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}

		return "false"
	case float64:
		return fmt.Sprintf("%v", typed)
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}

		return string(data)
	}
}

// runDB dispatches wt db <app> <query|exec>.
func runDB(args []string) error {
	cfg, rest, err := clientFlags(args, true)
	if err != nil {
		return err
	}

	if len(rest) < 2 {
		return errors.New("usage: wt db <app> <query|exec>")
	}

	name, action, actionArgs := rest[0], rest[1], rest[2:]

	switch action {
	case "query":
		return runDBQuery(cfg, name, actionArgs)
	case "exec":
		return runDBExec(cfg, name, actionArgs)
	default:
		return fmt.Errorf("unknown db action %q", action)
	}
}
