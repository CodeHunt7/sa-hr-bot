package db_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"sa-hr-bot/internal/db"
	"sa-hr-bot/internal/migrations"
)

// This test deliberately requires an explicit opt-in because it truncates the
// participant tables. CI runs it against its dedicated PostgreSQL service;
// normal local go test runs remain isolated from a developer's database.
func TestSubscriptionRegistrationAndReportsPostgreSQL(t *testing.T) {
	if os.Getenv("RUN_DB_INTEGRATION_TESTS") != "1" {
		t.Skip("set RUN_DB_INTEGRATION_TESTS=1 with a disposable DATABASE_URL")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("DATABASE_URL is required for integration test")
	}

	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer sqlDB.Close()
	if err := migrations.Up(sqlDB); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := sqlDB.Exec(`TRUNCATE students CASCADE`); err != nil {
		t.Fatalf("truncate participant tables: %v", err)
	}

	ctx := context.Background()
	pool, err := db.NewPool(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	repo := db.NewRepository(pool)
	const activeID, newID = 12345, 12346
	if err := repo.RegisterStudent(ctx, activeID, "Тест"); err != nil {
		t.Fatalf("register without an access code: %v", err)
	}
	if err := repo.RegisterStudent(ctx, activeID, "Дубликат"); err != nil {
		t.Fatalf("repeat registration: %v", err)
	}
	if err := repo.RegisterStudent(ctx, newID, "Новый"); err != nil {
		t.Fatalf("register second student: %v", err)
	}

	student, err := repo.GetStudentByTelegramID(ctx, activeID)
	if err != nil || student.Name != "Тест" || student.TelegramID != activeID {
		t.Fatalf("load original student: student=%+v err=%v", student, err)
	}
	session, err := repo.StartSession(ctx, activeID)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := repo.SaveLLMUsage(ctx, activeID, session.ID, "primary_feedback", 100, 20, 120, 40); err != nil {
		t.Fatalf("save usage: %v", err)
	}
	report, err := repo.GetStudentReport(ctx, activeID)
	if err != nil || report.TelegramID != activeID || report.Calls != 1 || report.PromptTokens != 100 ||
		report.CompletionTokens != 20 || report.TotalTokens != 120 || report.CachedTokens != 40 || report.LastSessionAt == nil {
		t.Fatalf("load usage by Telegram ID: report=%+v err=%v", report, err)
	}
	reports, err := repo.GetStudentReports(ctx)
	if err != nil || len(reports) != 2 || reports[0].TotalTokens != 120 ||
		reports[1].TelegramID != newID || reports[1].TotalTokens != 0 || reports[1].LastSessionAt != nil {
		t.Fatalf("load both students, including one without usage: reports=%+v err=%v", reports, err)
	}
	if _, err := repo.GetStudentReport(ctx, 999); !errors.Is(err, db.ErrStudentNotFound) {
		t.Fatalf("unknown Telegram ID: got %v, want ErrStudentNotFound", err)
	}
}
