package main

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStorageMigrationChecksums(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("checks")

	migrate := func(version int, statements []map[string]any) (int, storageMigrationResponse, storageErrorResponse) {
		t.Helper()
		resp := storageSQL(t, s, token, "/v1/migrate", map[string]any{"version": version, "statements": statements})
		defer resp.Body.Close()
		var result storageMigrationResponse
		var failure storageErrorResponse
		if resp.StatusCode == http.StatusOK {
			if err := json.UnmarshalRead(resp.Body, &result); err != nil {
				t.Fatal(err)
			}
		} else if err := json.UnmarshalRead(resp.Body, &failure); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, result, failure
	}
	schema := func(stats bool) storageSchema {
		t.Helper()
		target := s.url() + "/v1/schema"
		if stats {
			target += "?stats=1"
		}
		resp := storageDo(t, http.MethodGet, target, token, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("schema: %d %s", resp.StatusCode, readBody(t, resp))
		}
		defer resp.Body.Close()
		var result storageSchema
		if err := json.UnmarshalRead(resp.Body, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	first := []map[string]any{{"sql": `create table checks (id integer primary key, value text)`}}
	status, result, _ := migrate(1, first)
	if status != http.StatusOK || !result.Applied || result.Version != 1 {
		t.Fatalf("first migration = %d %+v", status, result)
	}
	schemaWithChecksum := schema(false)
	if len(schemaWithChecksum.Migrations) != 1 || schemaWithChecksum.Migrations[0].Version != 1 ||
		len(schemaWithChecksum.Migrations[0].Checksum) != 64 || schemaWithChecksum.Migrations[0].AppliedAt == "" {
		t.Errorf("migrations = %+v", schemaWithChecksum.Migrations)
	}

	status, result, _ = migrate(1, first)
	if status != http.StatusOK || result.Applied {
		t.Errorf("same-version migration = %d %+v, want a no-op", status, result)
	}

	status, _, failure := migrate(1, []map[string]any{{"sql": `create table edited (id integer primary key)`}})
	if status != http.StatusConflict || failure.Code != "migration_checksum" || !strings.Contains(failure.Error, "version 1") {
		t.Errorf("edited migration = %d %+v, want a 409 migration_checksum error", status, failure)
	}

	status, result, _ = migrate(2, []map[string]any{{"sql": `create table extra (id integer primary key)`}})
	if status != http.StatusOK || !result.Applied || result.Version != 2 {
		t.Errorf("new migration = %d %+v", status, result)
	}
	schemaWithChecksum = schema(false)
	if len(schemaWithChecksum.Migrations) != 2 ||
		schemaWithChecksum.Migrations[0].Version != 1 || schemaWithChecksum.Migrations[1].Version != 2 {
		t.Errorf("migrations after version 2 = %+v", schemaWithChecksum.Migrations)
	}
	for _, table := range schemaWithChecksum.Tables {
		if isStorageMigrationsTable(table.Name) {
			t.Error("schema listed the migration bookkeeping table")
		}
	}
	tables, err := s.databaseTables(context.Background(), "checks")
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		if isStorageMigrationsTable(table) {
			t.Error("database table listing included the migration bookkeeping table")
		}
	}
	if _, err := s.databaseRows(context.Background(), "checks", storageMigrationsTable, 10, 0); !errors.Is(err, errDatabaseTableNotFound) {
		t.Errorf("databaseRows(wt_migrations) error = %v, want table not found", err)
	}

	for _, statement := range []string{
		`insert into wt_migrations (version, checksum, applied_at) values (9, 'x', 'now')`,
		`drop table wt_migrations`,
	} {
		resp := storageSQL(t, s, token, "/v1/exec", map[string]any{"sql": statement})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("app statement %q = %d %s, want 403", statement, resp.StatusCode, readBody(t, resp))
		}
		resp.Body.Close()
	}
}

func TestStorageLegacyMigrationVersionWithoutChecksum(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("legacy")

	ctx := context.Background()
	if err := s.withConn(ctx, "legacy", func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `create table legacy_items (id integer primary key)`); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `pragma user_version = 2`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	schemaOf := func() storageSchema {
		t.Helper()
		resp := storageDo(t, http.MethodGet, s.url()+"/v1/schema", token, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("schema: %d %s", resp.StatusCode, readBody(t, resp))
		}
		defer resp.Body.Close()
		var result storageSchema
		if err := json.UnmarshalRead(resp.Body, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	schema := schemaOf()
	if schema.Version != 2 || len(schema.Migrations) != 0 {
		t.Fatalf("legacy schema = %+v", schema)
	}

	resp := storageSQL(t, s, token, "/v1/migrate", map[string]any{
		"version":    2,
		"statements": []map[string]any{{"sql": `create table must_not_exist (id integer primary key)`}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy no-op = %d %s", resp.StatusCode, readBody(t, resp))
	}
	var result storageMigrationResponse
	if err := json.UnmarshalRead(resp.Body, &result); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if result.Applied {
		t.Error("legacy same-version migration was applied")
	}
	if schema = schemaOf(); len(schema.Migrations) != 0 {
		t.Errorf("legacy no-op invented checksums: %+v", schema.Migrations)
	}

	resp = storageSQL(t, s, token, "/v1/migrate", map[string]any{
		"version":    3,
		"statements": []map[string]any{{"sql": `create table new_items (id integer primary key)`}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new migration = %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	schema = schemaOf()
	if schema.Version != 3 || len(schema.Migrations) != 1 || schema.Migrations[0].Version != 3 {
		t.Errorf("schema after new version = %+v", schema)
	}
}

func TestStorageSchemaDetailsAndStats(t *testing.T) {
	s, err := newStorageServer(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	token := s.tokenFor("relations")

	statements := []map[string]any{
		{"sql": `create table parents (id integer primary key, name text not null)`},
		{"sql": `create table children (id integer primary key, parent_id integer not null references parents(id) on delete cascade, note text)`},
		{"sql": `create index children_parent on children(parent_id)`},
		{"sql": `create unique index children_note_unique on children(note) where note is not null`},
		{"sql": `create index children_lower_note on children(lower(note))`},
		{"sql": `create view children_notes as select id, note from children`},
	}
	resp := storageSQL(t, s, token, "/v1/migrate", map[string]any{"version": 1, "statements": statements})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("migrate: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	resp = storageSQL(t, s, token, "/v1/exec", map[string]any{
		"sql": `insert into parents (id, name) values (1, 'one')`,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("insert parent: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()
	for i := range 7 {
		resp = storageSQL(t, s, token, "/v1/exec", map[string]any{
			"sql":    `insert into children (id, parent_id, note) values (?, 1, ?)`,
			"params": []any{i + 1, nil},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("insert child %d: %d %s", i+1, resp.StatusCode, readBody(t, resp))
		}
		resp.Body.Close()
	}

	schemaOf := func(stats bool) storageSchema {
		t.Helper()
		target := s.url() + "/v1/schema"
		if stats {
			target += "?stats=1"
		}
		resp := storageDo(t, http.MethodGet, target, token, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("schema: %d %s", resp.StatusCode, readBody(t, resp))
		}
		defer resp.Body.Close()
		var result storageSchema
		if err := json.UnmarshalRead(resp.Body, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	plain := schemaOf(false)
	for _, table := range plain.Tables {
		if table.Rows != nil || table.Bytes != nil {
			t.Errorf("plain schema included stats for %s: rows=%v bytes=%v", table.Name, table.Rows, table.Bytes)
		}
	}

	full := schemaOf(true)
	var children *storageTable
	for i := range full.Tables {
		if full.Tables[i].Name == "children" {
			children = &full.Tables[i]
		}
	}
	if children == nil {
		t.Fatalf("children table missing: %+v", full.Tables)
	}
	if children.Rows == nil || *children.Rows != 7 {
		t.Errorf("children rows = %v, want 7", children.Rows)
	}
	if children.Bytes == nil || *children.Bytes < 0 {
		t.Errorf("children bytes = %v", children.Bytes)
	}

	indexOf := func(name string) *storageIndex {
		for i := range children.Indexes {
			if children.Indexes[i].Name == name {
				return &children.Indexes[i]
			}
		}
		return nil
	}
	parentIndex := indexOf("children_parent")
	if parentIndex == nil || len(parentIndex.Columns) != 1 || parentIndex.Columns[0] == nil || *parentIndex.Columns[0] != "parent_id" {
		t.Errorf("parent index = %+v", parentIndex)
	}
	uniqueIndex := indexOf("children_note_unique")
	if uniqueIndex == nil || !uniqueIndex.Unique || !uniqueIndex.Partial || uniqueIndex.Origin != "c" {
		t.Errorf("unique index = %+v", uniqueIndex)
	}
	expressionIndex := indexOf("children_lower_note")
	if expressionIndex == nil || len(expressionIndex.Columns) != 1 || expressionIndex.Columns[0] != nil {
		t.Errorf("expression index = %+v", expressionIndex)
	}

	if len(children.ForeignKeys) != 1 {
		t.Fatalf("foreign keys = %+v", children.ForeignKeys)
	}
	foreignKey := children.ForeignKeys[0]
	if foreignKey.Table != "parents" || foreignKey.From != "parent_id" || foreignKey.To == nil || *foreignKey.To != "id" ||
		foreignKey.OnDelete != "CASCADE" || foreignKey.OnUpdate != "NO ACTION" || foreignKey.Match != "NONE" {
		t.Errorf("foreign key = %+v", foreignKey)
	}

	if len(full.Views) != 1 || full.Views[0].Name != "children_notes" || !strings.Contains(full.Views[0].SQL, "children_notes") {
		t.Errorf("views = %+v", full.Views)
	}
	if len(full.Views) == 1 && len(full.Views[0].Columns) != 2 {
		t.Errorf("view columns = %+v", full.Views[0].Columns)
	}
	if len(full.Migrations) != 1 || full.Migrations[0].Version != 1 || len(full.Migrations[0].Checksum) != 64 {
		t.Errorf("migrations = %+v", full.Migrations)
	}
}

func TestStorageMigrationTimeoutConfig(t *testing.T) {
	timeout, err := parseStorageMigrationTimeout("")
	if err != nil || timeout != 5*time.Minute {
		t.Errorf("default timeout = %s, %v; want 5m", timeout, err)
	}
	for _, value := range []string{"1s", "10m", "1h"} {
		if _, err := parseStorageMigrationTimeout(value); err != nil {
			t.Errorf("parseStorageMigrationTimeout(%q) = %v", value, err)
		}
	}
	for _, value := range []string{"500ms", "2h", "soon"} {
		if _, err := parseStorageMigrationTimeout(value); err == nil {
			t.Errorf("parseStorageMigrationTimeout(%q) accepted an invalid value", value)
		}
	}

	t.Setenv("WT_STORAGE_MIGRATION_TIMEOUT", "20m")
	if got := envStorageMigrationTimeout(); got != 20*time.Minute {
		t.Errorf("config timeout = %s, want 20m", got)
	}
	t.Setenv("WT_STORAGE_MIGRATION_TIMEOUT", "invalid")
	if got := envStorageMigrationTimeout(); got != defaultStorageMigrationTimeout {
		t.Errorf("invalid config timeout = %s, want default", got)
	}
}
