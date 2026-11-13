package storage

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// QuoteIdentifier quotes a SQLite identifier such as a table or column name, doubling any embedded quotes.
func QuoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// queryStrings runs a query whose first column is text and collects it.
func (c *Client) queryStrings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := c.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string

	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}

		out = append(out, value)
	}

	return out, rows.Err()
}

// Columns returns the column names of table in declaration order, or nil when the table does not exist.
func (c *Client) Columns(ctx context.Context, table string) ([]string, error) {
	return c.queryStrings(ctx, `select name from pragma_table_info(?)`, table)
}

// HasColumn reports whether table exists and has column. SQLite compares column names case-insensitively, and so does HasColumn.
func (c *Client) HasColumn(ctx context.Context, table, column string) (bool, error) {
	columns, err := c.Columns(ctx, table)
	if err != nil {
		return false, err
	}

	for _, name := range columns {
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}

	return false, nil
}

// AddColumnIfMissing adds column to an existing table that lacks it, with definition as the type and constraints, such as `text not null default ”`. It reports whether it added the column. A missing table is left alone, because its create statement already holds every column.
//
// Run it after Migrate rather than inside a versioned statement set: the alter only appears on databases that need it, so putting it in the set would change that version's checksum between databases and deploys.
func (c *Client) AddColumnIfMissing(ctx context.Context, table, column, definition string) (bool, error) {
	columns, err := c.Columns(ctx, table)
	if err != nil {
		return false, err
	}

	if len(columns) == 0 {
		return false, nil
	}

	for _, name := range columns {
		if strings.EqualFold(name, column) {
			return false, nil
		}
	}

	statement := fmt.Sprintf("alter table %s add column %s %s", QuoteIdentifier(table), QuoteIdentifier(column), strings.TrimSpace(definition))
	if _, err := c.Exec(ctx, statement); err != nil {
		return false, err
	}

	return true, nil
}

// TableSQL returns the create statement SQLite stored for table. ok is false when the table does not exist.
func (c *Client) TableSQL(ctx context.Context, table string) (definition string, ok bool, err error) {
	err = c.QueryRow(ctx, `select sql from sqlite_master where type = 'table' and name = ? collate nocase`, table).Scan(&definition)
	if errors.Is(err, ErrNoRows) {
		return "", false, nil
	}

	if err != nil {
		return "", false, err
	}

	return definition, true, nil
}

// TableRebuild describes a table change SQLite's alter table cannot express, such as a new unique key or a changed check constraint.
type TableRebuild struct {
	// Table is the existing table to replace.
	Table string
	// Create creates the replacement under the name TempTable returns, with the full new definition.
	Create string
	// Copy fills the replacement from Table, such as `insert into t_new (a, b) select a, coalesce(b, '') from t`.
	Copy string
	// After runs once the replacement has taken Table's name, for its indexes and triggers, which the drop removed.
	After []string
}

// TempTable is the name the replacement table is created under before the swap.
func (r TableRebuild) TempTable() string {
	return r.Table + "_new"
}

// errRebuildReferenced stops a rebuild that would cascade through other tables' foreign keys.
var errRebuildReferenced = errors.New("storage: table is referenced by foreign keys")

// rebuildReferences lists the other tables whose foreign keys point at table. Dropping table with foreign keys enforced would run their on delete actions. The storage authoriser allows pragma functions only on their own, so each table is read separately.
func (c *Client) rebuildReferences(ctx context.Context, table string) ([]string, error) {
	tables, err := c.queryStrings(ctx, `select name from sqlite_master where type = 'table' and name not like 'sqlite\_%' escape '\' and name <> 'wt_migrations' order by name`)
	if err != nil {
		return nil, err
	}

	var names []string

	for _, name := range tables {
		if strings.EqualFold(name, table) {
			continue
		}

		targets, err := c.queryStrings(ctx, `select "table" from pragma_foreign_key_list(?)`, name)
		if err != nil {
			return nil, err
		}

		if slices.ContainsFunc(targets, func(target string) bool { return strings.EqualFold(target, table) }) {
			names = append(names, name)
		}
	}

	return names, nil
}

// RebuildTable replaces a table with a new definition in one transaction: create the replacement, copy the rows, drop the old table, rename the replacement and run After. A missing table is left alone, so it is safe to call on every start once guarded by a check such as TableSQL.
//
// The storage service enforces foreign keys and cannot turn them off for the swap, so RebuildTable refuses a table that other tables reference: dropping it would run their on delete actions. Rebuild those tables in a coordinated migration instead.
func (c *Client) RebuildTable(ctx context.Context, rebuild TableRebuild) error {
	if rebuild.Table == "" || rebuild.Create == "" || rebuild.Copy == "" {
		return errors.New("storage: RebuildTable needs Table, Create and Copy")
	}

	if _, ok, err := c.TableSQL(ctx, rebuild.Table); err != nil || !ok {
		return err
	}

	references, err := c.rebuildReferences(ctx, rebuild.Table)
	if err != nil {
		return err
	}

	if len(references) > 0 {
		return fmt.Errorf("%w: %s is referenced by %s", errRebuildReferenced, rebuild.Table, strings.Join(references, ", "))
	}

	statements := []TransactionStatement{
		{SQL: rebuild.Create},
		{SQL: rebuild.Copy},
		{SQL: "drop table " + QuoteIdentifier(rebuild.Table)},
		{SQL: fmt.Sprintf("alter table %s rename to %s", QuoteIdentifier(rebuild.TempTable()), QuoteIdentifier(rebuild.Table))},
	}
	for _, statement := range rebuild.After {
		statements = append(statements, TransactionStatement{SQL: statement})
	}
	// Transaction splits large calls into separate commits, which would expose a half-swapped table.
	if len(statements) > MaxTransactionStatements {
		return fmt.Errorf("storage: RebuildTable has %d statements, more than the %d one transaction allows", len(statements), MaxTransactionStatements)
	}

	_, err = c.Transaction(ctx, statements...)

	return err
}
