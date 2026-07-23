// Command import-questions synchronizes question_bank.csv with PostgreSQL.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/joho/godotenv"

	"sa-hr-bot/internal/db"
	"sa-hr-bot/internal/questionbank"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	csvPath := flag.String("file", "question_bank.csv", "path to the question bank CSV file")
	flag.Parse()

	_ = godotenv.Load()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}

	ctx := context.Background()
	pool, err := db.NewPool(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	count, err := questionbank.Sync(ctx, pool, *csvPath)
	if err != nil {
		return err
	}
	fmt.Printf("Загружено вопросов: %d\n", count)
	return nil
}
