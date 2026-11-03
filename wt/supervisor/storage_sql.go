package main

import (
	"fmt"
	"strings"
)

// validateStorageSQL only checks that a statement is present and within the size limit. Whether a statement is safe is decided by SQLite's authoriser when it is prepared, so identifiers that merely look like blocked pragmas or functions remain valid.
func validateStorageSQL(query string) error {
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("%w: sql must not be empty", errStorageInvalid)
	}

	if len(query) > maxStorageSQL {
		return fmt.Errorf("%w: sql is too long (max %d bytes)", errStorageInvalid, maxStorageSQL)
	}

	return nil
}
