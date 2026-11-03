package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"modernc.org/sqlite"
)

var errStorageAuthoriserUnsupported = errors.New("sqlite driver does not support an authoriser")

// safeStorageVTables are the virtual-table modules an app may create. Anything else could load a module that reaches outside the database file.
var safeStorageVTables = map[string]bool{
	"fts5":      true,
	"fts5vocab": true,
	"rtree":     true,
	"rtree_i32": true,
}

// deniedStorageFunctions can reach the filesystem or load native code, so no app statement may call them.
var deniedStorageFunctions = map[string]bool{
	"load_extension": true,
	"readfile":       true,
	"writefile":      true,
	"edit":           true,
}

// alwaysAllowedStoragePragmas are read-only host pragmas SQLite itself runs while compiling some statements; FTS5 writes read data_version. They cannot change the database.
var alwaysAllowedStoragePragmas = map[string]bool{
	"data_version": true,
}

// safeReadStoragePragmas are the pragmas whose table-valued function form only reads. They are allowed only when the authoriser has just seen the matching pragma_ virtual-table read, so an actual PRAGMA statement stays denied.
var safeReadStoragePragmas = map[string]bool{
	"collation_list":    true,
	"compile_options":   true,
	"database_list":     true,
	"foreign_key_check": true,
	"foreign_key_list":  true,
	"freelist_count":    true,
	"function_list":     true,
	"index_info":        true,
	"index_list":        true,
	"index_xinfo":       true,
	"integrity_check":   true,
	"module_list":       true,
	"page_count":        true,
	"page_size":         true,
	"pragma_list":       true,
	"quick_check":       true,
	"table_info":        true,
	"table_list":        true,
	"table_xinfo":       true,
}

// readOnlyStorageWriteActions are denied in read-only mode. Write mode allows them because writes have already been routed to the write pool and the write endpoint.
var readOnlyStorageWriteActions = map[sqlite.AuthorizerActionCode]bool{
	sqlite.AuthInsert:            true,
	sqlite.AuthUpdate:            true,
	sqlite.AuthDelete:            true,
	sqlite.AuthCreateTable:       true,
	sqlite.AuthCreateIndex:       true,
	sqlite.AuthCreateTrigger:     true,
	sqlite.AuthCreateView:        true,
	sqlite.AuthCreateTempTable:   true,
	sqlite.AuthCreateTempIndex:   true,
	sqlite.AuthCreateTempTrigger: true,
	sqlite.AuthCreateTempView:    true,
	sqlite.AuthCreateVTable:      true,
	sqlite.AuthDropTable:         true,
	sqlite.AuthDropIndex:         true,
	sqlite.AuthDropTrigger:       true,
	sqlite.AuthDropView:          true,
	sqlite.AuthDropTempTable:     true,
	sqlite.AuthDropTempIndex:     true,
	sqlite.AuthDropTempTrigger:   true,
	sqlite.AuthDropTempView:      true,
	sqlite.AuthDropVTable:        true,
	sqlite.AuthAlterTable:        true,
	sqlite.AuthReindex:           true,
	sqlite.AuthAnalyze:           true,
}

// storageInternalMutation reports whether an app statement writes or changes the schema of the reserved migration table. Reads stay visible to the authoriser, but the table is hidden from every app-facing listing.
func storageInternalMutation(action sqlite.AuthorizerActionCode, arg1, arg2 string) bool {
	switch action {
	case sqlite.AuthCreateIndex, sqlite.AuthCreateTempIndex, sqlite.AuthDropIndex, sqlite.AuthDropTempIndex,
		sqlite.AuthCreateTrigger, sqlite.AuthCreateTempTrigger, sqlite.AuthDropTrigger, sqlite.AuthDropTempTrigger,
		sqlite.AuthAlterTable:
		// CREATE/DROP INDEX and TRIGGER report the object name first and the table second.
		return isStorageMigrationsTable(arg2)
	case sqlite.AuthInsert, sqlite.AuthUpdate, sqlite.AuthDelete,
		sqlite.AuthCreateTable, sqlite.AuthCreateTempTable, sqlite.AuthDropTable, sqlite.AuthDropTempTable,
		sqlite.AuthCreateView, sqlite.AuthCreateTempView, sqlite.AuthDropView, sqlite.AuthDropTempView,
		sqlite.AuthCreateVTable, sqlite.AuthDropVTable, sqlite.AuthReindex, sqlite.AuthAnalyze:
		return isStorageMigrationsTable(arg1)
	default:
		return false
	}
}

// storageAuthoriser is the per-connection policy. It denies actions that can reach outside the app database, and in read-only mode it denies every write and schema change.
func storageAuthoriser(readOnly bool) sqlite.AuthorizerFn {
	// A table-valued pragma function such as pragma_table_info presents the same AuthPragma action as a PRAGMA statement. Track the table-function read that precedes it so reads keep working while actual PRAGMA statements stay denied.
	inPragmaTableFunction := false

	return func(action sqlite.AuthorizerActionCode, arg1, arg2, _, _ string) sqlite.AuthorizerReturnCode {
		if storageInternalMutation(action, arg1, arg2) {
			return sqlite.AuthorizerDeny
		}

		switch action {
		case sqlite.AuthSelect:
			inPragmaTableFunction = false
		case sqlite.AuthRead:
			if strings.HasPrefix(strings.ToLower(arg1), "pragma_") {
				inPragmaTableFunction = true
			}
		case sqlite.AuthPragma:
			name := strings.ToLower(arg1)
			if alwaysAllowedStoragePragmas[name] {
				return sqlite.AuthorizerOK
			}

			if inPragmaTableFunction {
				inPragmaTableFunction = false

				if safeReadStoragePragmas[name] {
					return sqlite.AuthorizerOK
				}
			}

			return sqlite.AuthorizerDeny
		case sqlite.AuthAttach, sqlite.AuthDetach, sqlite.AuthTransaction, sqlite.AuthSavepoint:
			return sqlite.AuthorizerDeny
		case sqlite.AuthFunction:
			if deniedStorageFunctions[strings.ToLower(arg2)] {
				return sqlite.AuthorizerDeny
			}
		case sqlite.AuthCreateVTable:
			// In modernc, CREATE VIRTUAL TABLE reports the table name as arg1 and the module name as arg2.
			if !safeStorageVTables[strings.ToLower(arg2)] {
				return sqlite.AuthorizerDeny
			}
		default:
			// Other actions fall through to the read-only write check below.
		}

		if readOnly && readOnlyStorageWriteActions[action] {
			return sqlite.AuthorizerDeny
		}

		return sqlite.AuthorizerOK
	}
}

// registerStorageAuthoriser installs a policy on a raw driver connection. It fails closed when the driver cannot enforce one.
func registerStorageAuthoriser(driverConn any, fn sqlite.AuthorizerFn) error {
	registerer, ok := driverConn.(sqlite.AuthorizerRegisterer)
	if !ok {
		return errStorageAuthoriserUnsupported
	}

	return registerer.RegisterAuthorizer(fn)
}

// installStorageAuthoriser registers a policy on one database/sql connection and returns the function that removes it.
func installStorageAuthoriser(conn *sql.Conn, readOnly bool) (func() error, error) {
	err := conn.Raw(func(driverConn any) error {
		return registerStorageAuthoriser(driverConn, storageAuthoriser(readOnly))
	})
	if err != nil {
		return nil, err
	}

	return func() error {
		return conn.Raw(func(driverConn any) error {
			return registerStorageAuthoriser(driverConn, nil)
		})
	}, nil
}

// unregisterStorageAuthoriser removes any policy from the connection. It is used when a session outlives the install call.
func unregisterStorageAuthoriser(conn *sql.Conn) error {
	return conn.Raw(func(driverConn any) error {
		return registerStorageAuthoriser(driverConn, nil)
	})
}

// requireStorageAuthoriser installs the SQL authoriser on one connection and returns its uninstall function.
func requireStorageAuthoriser(conn *sql.Conn, readOnly bool) (func() error, error) {
	unregister, err := installStorageAuthoriser(conn, readOnly)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errStorageUnavailable, err)
	}

	return unregister, nil
}
