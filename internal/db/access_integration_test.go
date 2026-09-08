package db_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"sa-hr-bot/internal/db"
	"sa-hr-bot/internal/migrations"
)

// This test deliberately requires an explicit opt-in because it truncates the
// participant tables. CI runs it against its dedicated PostgreSQL service;
// normal local go test runs remain isolated from a developer's database.
func TestAccessActivationAndRenewalPostgreSQL(t *testing.T) {
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
	if _, err := sqlDB.Exec(`TRUNCATE access_codes CASCADE`); err != nil {
		t.Fatalf("truncate access tables: %v", err)
	}

	ctx := context.Background()
	pool, err := db.NewPool(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	repo := db.NewRepository(pool)
	codes, err := repo.CreateAccessCodes(ctx, 2)
	if err != nil {
		t.Fatalf("create access codes: %v", err)
	}

	student, err := repo.CreateStudentIfAccessCodeValid(ctx, 12345, "Тест", codes[0])
	if err != nil {
		t.Fatalf("activate first code: %v", err)
	}
	if !student.AccessExpiresAt.Equal(student.AccessActivatedAt.AddDate(0, 2, 0)) {
		t.Fatalf("first access interval is not two calendar months: activated=%s expires=%s", student.AccessActivatedAt, student.AccessExpiresAt)
	}

	renewed, err := repo.RenewStudentAccessWithCode(ctx, student.TelegramID, codes[1])
	if err != nil {
		t.Fatalf("renew access: %v", err)
	}
	if renewed.AccessCode != codes[1] || !renewed.AccessExpiresAt.Equal(renewed.AccessActivatedAt.AddDate(0, 2, 0)) {
		t.Fatalf("unexpected renewed access: %+v", renewed)
	}
	loaded, err := repo.GetStudentByTelegramID(ctx, student.TelegramID)
	if err != nil {
		t.Fatalf("load renewed student: %v", err)
	}
	if loaded.AccessCode != codes[1] || !loaded.AccessExpiresAt.Equal(renewed.AccessExpiresAt) {
		t.Fatalf("renewed access was not persisted: %+v", loaded)
	}
	codeReport, err := repo.GetTokenUsageByAccessCode(ctx, codes[1])
	if err != nil || codeReport.ActivatedAt == nil || codeReport.ExpiresAt == nil {
		t.Fatalf("load access-code dates: report=%+v err=%v", codeReport, err)
	}
	codeReports, err := repo.ListAccessCodes(ctx)
	if err != nil || len(codeReports) != 2 {
		t.Fatalf("list access codes: reports=%+v err=%v", codeReports, err)
	}
	studentReports, err := repo.GetStudentReports(ctx)
	if err != nil || len(studentReports) != 1 || !studentReports[0].AccessExpiresAt.Equal(renewed.AccessExpiresAt) {
		t.Fatalf("load student access report: reports=%+v err=%v", studentReports, err)
	}
	if _, err := repo.RenewStudentAccessWithCode(ctx, student.TelegramID, codes[0]); err != db.ErrAccessCodeInvalid {
		t.Fatalf("used code renewal error = %v, want ErrAccessCodeInvalid", err)
	}
}
