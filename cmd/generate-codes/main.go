// Command generate-codes creates unique, unused student access codes and
// inserts them into the access_codes table.
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
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	n := flag.Int("n", 20, "number of access codes to generate")
	flag.Parse()

	if *n <= 0 {
		return errors.New("-n must be a positive integer")
	}

	// In production DATABASE_URL is injected directly; .env is only for
	// local development and may not exist.
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

	codes, err := db.NewRepository(pool).CreateAccessCodes(ctx, *n)
	if err != nil {
		return err
	}

	for _, code := range codes {
		fmt.Println(code)
	}
	fmt.Printf("\nСгенерировано кодов: %d\n", len(codes))

	return nil
}
