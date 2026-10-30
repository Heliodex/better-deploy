package app

import (
	"context"
	"strings"

	"wt.packages/storage"
)

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// SeedOnce runs seed unless the marker is set. A table that already has rows is left alone and only marked seeded, so demo data never lands in a real database.
func SeedOnce(ctx context.Context, store *storage.Client, table string, seed func(context.Context) error) error {
	if _, err := store.Exec(ctx, `create table if not exists meta (key text primary key, value text not null)`); err != nil {
		return err
	}
	var seeded int64
	if err := store.QueryRow(ctx, `select count(*) from meta where key = 'seeded'`).Scan(&seeded); err != nil {
		return err
	}
	if seeded > 0 {
		return nil
	}
	var count int64
	if err := store.QueryRow(ctx, `select count(*) from `+quoteIdentifier(table)).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if err := seed(ctx); err != nil {
			return err
		}
	}
	_, err := store.Exec(ctx, `insert or ignore into meta (key, value) values ('seeded', '1')`)
	return err
}
