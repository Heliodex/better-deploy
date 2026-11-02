package storage

import (
	"errors"
	"fmt"
)

// Next advances to the next row.
func (r *Rows) Next() bool {
	if r.closed || r.err != nil || r.index >= len(r.rows) {
		return false
	}

	r.index++

	return true
}

// NextPage returns the following offset page when the last page was truncated, and false at the end. It only applies to rows returned by QueryPage.
//
// Offset pagination is not stable under concurrent writes, so use a deterministic ORDER BY and expect rows to move between pages.
func (r *Rows) NextPage() (Page, bool) {
	if r == nil || !r.Truncated || r.page.Limit <= 0 || len(r.rows) == 0 {
		return Page{}, false
	}

	next := r.page
	// Advance by the rows actually returned. The server may cap a page below the
	// requested limit, and skipping its full requested width would lose rows.
	next.Offset += len(r.rows)

	return next, true
}

// Close releases the rows. It exists so callers can use defer rows.Close().
func (r *Rows) Close() error {
	r.closed = true
	r.rows = nil

	return nil
}

// Err returns the error that stopped iteration, if any.
func (r *Rows) Err() error {
	return r.err
}

// Scan copies the current row into dest. It supports *string, *[]byte, *int64, *int, *float64, *bool, *time.Time, *any and anything with a Scan(any) error method.
func (r *Rows) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}

	if r.closed {
		return errors.New("storage: rows are closed")
	}

	if r.index == 0 || r.index > len(r.rows) {
		return ErrNoRows
	}

	if len(dest) != len(r.Columns) {
		return fmt.Errorf("storage: scan %d destinations into %d columns", len(dest), len(r.Columns))
	}

	row := r.rows[r.index-1]
	for i, target := range dest {
		if err := scanValue(target, row[i]); err != nil {
			return fmt.Errorf("storage: column %q: %w", r.Columns[i], err)
		}
	}

	return nil
}

// Scan copies the first row into dest, or returns ErrNoRows.
func (r *Row) Scan(dest ...any) error {
	if r.rows == nil {
		return ErrNoRows
	}

	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}

		return ErrNoRows
	}

	if err := r.rows.Scan(dest...); err != nil {
		return err
	}

	return r.rows.Close()
}
