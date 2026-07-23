// Command bot is the entry point for the technical mock-interview
// Telegram bot.
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	tele "gopkg.in/telebot.v3"

	"sa-hr-bot/internal/config"
	"sa-hr-bot/internal/db"
	"sa-hr-bot/internal/handlers"
	"sa-hr-bot/internal/llm"
	"sa-hr-bot/internal/migrations"
	"sa-hr-bot/internal/questionbank"
)

const shutdownDrainTimeout = 95 * time.Second

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := run(logger); err != nil {
		logger.Error("bot exited with error", "error", redactConfiguredSecrets(err.Error()))
		os.Exit(1)
	}
}

// redactConfiguredSecrets prevents SDK/network errors from printing tokens or
// database credentials embedded in request URLs. Some Telegram errors include
// the complete bot token in their URL.
func redactConfiguredSecrets(message string) string {
	for _, secret := range []string{
		os.Getenv("TELEGRAM_BOT_TOKEN"),
		os.Getenv("OPENAI_API_KEY"),
		os.Getenv("DATABASE_URL"),
	} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return message
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if err := applyMigrations(cfg.DatabaseURL); err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := db.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	llmClient := llm.New(llm.Config{
		APIKey:  cfg.OpenAIAPIKey,
		BaseURL: cfg.OpenAIBaseURL,
		Model:   cfg.OpenAIModel,
	})

	repo := db.NewRepository(pool)
	questionCount, err := questionbank.Sync(ctx, pool, "question_bank.csv")
	if err != nil {
		return err
	}
	logger.Info("question bank synchronized", "active_questions", questionCount)

	llmService, err := llm.NewService(llmClient, repo, logger, llm.ServiceConfig{
		PromptPath:        "prompts/system_prompt.md",
		SessionCycleLimit: cfg.SessionCycleLimit,
	})
	if err != nil {
		return err
	}

	h := handlers.New(repo, llmService, logger, cfg.SessionCycleLimit, cfg.AdminIDs, handlers.MediaConfig{
		KDIRVideoFileID: cfg.KDIRVideoFileID,
	})

	bot, err := tele.NewBot(tele.Settings{
		Token:  cfg.TelegramBotToken,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
		// Without this, telebot falls back to logging errors via the
		// standard "log" package instead of our structured logger,
		// which is how they were going unnoticed. See Handler.OnError.
		OnError: h.OnError,
	})
	if err != nil {
		return err
	}

	h.Register(bot)

	logger.Info("bot starting")
	shutdownSignal, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	go func() {
		<-shutdownSignal.Done()
		logger.Info("shutdown signal received, stopping polling")
		bot.Stop()
	}()
	bot.Start()

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancelDrain()
	if err := h.Shutdown(drainCtx); err != nil {
		logger.Warn("timed out waiting for handlers", "error", err)
	}
	logger.Info("bot stopped")

	return nil
}

// applyMigrations opens a plain database/sql connection (required by
// goose) to bring the schema up to date before the pgx pool used by
// the application takes over.
func applyMigrations(databaseURL string) error {
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	return migrations.Up(sqlDB)
}
