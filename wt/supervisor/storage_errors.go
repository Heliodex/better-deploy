package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// storageErrorResponse stays flat for existing clients: error is still the message, while code and the SQLite details are additive.
type storageErrorResponse struct {
	Error      string `json:"error"`
	Code       string `json:"code,omitempty"`
	SQLiteCode int    `json:"sqlite_code,omitempty"`
	Constraint string `json:"constraint,omitempty"`
}

// errStorageMigrationChecksum marks an edited migration whose version was already applied with a different checksum.
var errStorageMigrationChecksum = errors.New("migration checksum mismatch")

// storageSQLiteError maps an extended SQLite result code to an HTTP status and a stable error code.
func storageSQLiteError(code int) (status int, name, constraint string) {
	base := code & 0xff
	switch base {
	case sqlite3.SQLITE_CONSTRAINT:
		return http.StatusConflict, "constraint", storageConstraintName(code)
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return http.StatusServiceUnavailable, "busy", ""
	case sqlite3.SQLITE_FULL:
		return http.StatusInsufficientStorage, "disk_full", ""
	case sqlite3.SQLITE_READONLY:
		return http.StatusForbidden, "readonly", ""
	case sqlite3.SQLITE_AUTH, sqlite3.SQLITE_PERM:
		return http.StatusForbidden, "denied", ""
	case sqlite3.SQLITE_TOOBIG, sqlite3.SQLITE_RANGE, sqlite3.SQLITE_MISMATCH:
		return http.StatusBadRequest, "invalid", ""
	default:
		return http.StatusInternalServerError, "internal", ""
	}
}

// storageConstraintName names the extended constraint that failed, for clients that want to react to a specific one.
func storageConstraintName(code int) string {
	switch code {
	case sqlite3.SQLITE_CONSTRAINT_CHECK:
		return "CHECK"
	case sqlite3.SQLITE_CONSTRAINT_COMMITHOOK:
		return "COMMIT HOOK"
	case sqlite3.SQLITE_CONSTRAINT_DATATYPE:
		return "DATATYPE"
	case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
		return "FOREIGN KEY"
	case sqlite3.SQLITE_CONSTRAINT_FUNCTION:
		return "FUNCTION"
	case sqlite3.SQLITE_CONSTRAINT_NOTNULL:
		return "NOT NULL"
	case sqlite3.SQLITE_CONSTRAINT_PINNED:
		return "PINNED"
	case sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
		return "PRIMARY KEY"
	case sqlite3.SQLITE_CONSTRAINT_ROWID:
		return "ROWID"
	case sqlite3.SQLITE_CONSTRAINT_TRIGGER:
		return "TRIGGER"
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE:
		return "UNIQUE"
	case sqlite3.SQLITE_CONSTRAINT_VTAB:
		return "VIRTUAL TABLE"
	default:
		return "CONSTRAINT"
	}
}

// storageErrorDetails classifies an error from a storage handler. Unknown SQL and runtime failures stay 500; only request validation is 400.
func storageErrorDetails(err error) (int, storageErrorResponse) {
	switch {
	case errors.Is(err, errStorageInvalid):
		return http.StatusBadRequest, storageErrorResponse{Error: errorMessage(err), Code: "invalid"}
	case errors.Is(err, errStorageUnavailable):
		return http.StatusInternalServerError, storageErrorResponse{Error: errorMessage(err), Code: "storage_unavailable"}
	case errors.Is(err, errStorageMigrationChecksum):
		return http.StatusConflict, storageErrorResponse{Error: errorMessage(err), Code: "migration_checksum"}
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusGatewayTimeout, storageErrorResponse{Error: "storage query timed out", Code: "timeout"}
	}

	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		code := sqliteErr.Code()
		if code == sqlite3.SQLITE_ERROR && strings.Contains(strings.ToLower(sqliteErr.Error()), "not authorized to use function") {
			// SQLite refuses a disabled function such as load_extension before the authorizer runs, and reports SQLITE_ERROR.
			return http.StatusForbidden, storageErrorResponse{Error: errorMessage(err), Code: "denied", SQLiteCode: code}
		}
		status, name, constraint := storageSQLiteError(code)
		return status, storageErrorResponse{
			Error:      errorMessage(err),
			Code:       name,
			SQLiteCode: code,
			Constraint: constraint,
		}
	}
	return http.StatusInternalServerError, storageErrorResponse{Error: errorMessage(err), Code: "internal"}
}

func errorMessage(err error) string {
	return strings.TrimSpace(err.Error())
}

func writeStorageError(w http.ResponseWriter, err error) {
	status, body := storageErrorDetails(err)
	writeJSON(w, status, body)
}
