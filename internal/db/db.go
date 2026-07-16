// Package db provides the database connection pool and data models
// for the interview bot. PostgreSQL is used (see models.go doc comment
// for the reasoning behind that choice).
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool creates a PostgreSQL connection pool and verifies connectivity
// with a ping so that startup fails fast on misconfiguration.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create pgx pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}
