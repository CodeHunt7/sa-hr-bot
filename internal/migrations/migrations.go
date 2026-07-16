// Package migrations embeds the SQL migration files and applies them
// with goose at startup, so the binary is self-contained and does not
// depend on an external migration runner in production.
package migrations

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

//go:embed sql/*.sql
var embedFS embed.FS

// Up applies all pending migrations to the database reachable via db.
func Up(db *sql.DB) error {
	goose.SetBaseFS(embedFS)

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}

	if err := goose.Up(db, "sql"); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	return nil
}
