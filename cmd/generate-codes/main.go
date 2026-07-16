// Command generate-codes creates unique, unused student access codes and
// inserts them into the access_codes table.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

const (
	codePrefix    = "SA2026-"
	codeSuffixLen = 4

	// codeAlphabet excludes characters that are easy to confuse by eye:
	// 0/O and 1/I. Its length (32) is a power of two, which lets
	// randomCode map random bytes onto it with a bitmask and no modulo
	// bias.
	codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
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
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	codes := make([]string, 0, *n)
	for len(codes) < *n {
		code, err := randomCode()
		if err != nil {
			return err
		}

		// ON CONFLICT DO NOTHING + RETURNING tells us, atomically,
		// whether this code collided with one already in the table
		// (from this run or a previous one). On collision we just
		// generate another candidate.
		var inserted string
		err = pool.QueryRow(ctx,
			`INSERT INTO access_codes (code) VALUES ($1) ON CONFLICT (code) DO NOTHING RETURNING code`,
			code,
		).Scan(&inserted)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("insert access code: %w", err)
		}

		codes = append(codes, inserted)
	}

	for _, code := range codes {
		fmt.Println(code)
	}
	fmt.Printf("\nСгенерировано кодов: %d\n", len(codes))

	return nil
}

// randomCode generates one codePrefix + codeSuffixLen candidate using
// crypto/rand: access codes are credentials, so they must not be
// guessable the way a math/rand sequence could be.
func randomCode() (string, error) {
	raw := make([]byte, codeSuffixLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}

	suffix := make([]byte, codeSuffixLen)
	for i, b := range raw {
		suffix[i] = codeAlphabet[b&31]
	}

	return codePrefix + string(suffix), nil
}
