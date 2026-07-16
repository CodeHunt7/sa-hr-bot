// Command migrate applies all embedded Goose migrations without starting the
// Telegram bot. It is useful for local checks and deployment release steps.
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"

	"sa-hr-bot/internal/migrations"
)

func main() {
	_ = godotenv.Load()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(1)
	}

	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open database:", err)
		os.Exit(1)
	}
	defer database.Close()

	if err := migrations.Up(database); err != nil {
		fmt.Fprintln(os.Stderr, "apply migrations:", err)
		os.Exit(1)
	}
}
