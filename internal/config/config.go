// Package config loads application configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// defaultSessionCycleLimit is used when SESSION_CYCLE_LIMIT is not set.
const defaultSessionCycleLimit = 8

// Config holds all runtime configuration for the bot.
type Config struct {
	TelegramBotToken string
	OpenAIAPIKey     string
	OpenAIBaseURL    string // optional, used when routing OpenAI calls through a proxy
	OpenAIModel      string // optional, e.g. "gpt-4.1-mini"; empty means llm.New picks its default
	DatabaseURL      string
	KDIRVideoFileID  string // optional Telegram file_id; empty uploads media/vid1-kdir.mp4

	// SessionCycleLimit is the number of question cycles (system prompt
	// phase 3) per interview session. It is substituted into the
	// {{SESSION_CYCLE_LIMIT}} placeholder in prompts/system_prompt.md.
	SessionCycleLimit int

	// AdminIDs are the Telegram user IDs allowed to run /report. Empty
	// means nobody can (the command silently no-ops for everyone).
	AdminIDs []int64
}

// Load reads a .env file if present (local development) and then builds
// a Config from environment variables. Missing required variables result
// in an error so misconfiguration is caught at startup.
func Load() (*Config, error) {
	// Ignore the error: in production, env vars are usually injected
	// directly (Docker/systemd) and no .env file exists.
	_ = godotenv.Load()

	cfg := &Config{
		TelegramBotToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		OpenAIAPIKey:     os.Getenv("OPENAI_API_KEY"),
		OpenAIBaseURL:    os.Getenv("OPENAI_BASE_URL"),
		OpenAIModel:      os.Getenv("OPENAI_MODEL"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		KDIRVideoFileID:  os.Getenv("KDIR_VIDEO_FILE_ID"),
	}

	if cfg.TelegramBotToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}
	if cfg.OpenAIAPIKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is required")
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	cfg.SessionCycleLimit = defaultSessionCycleLimit
	if v := os.Getenv("SESSION_CYCLE_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("SESSION_CYCLE_LIMIT must be a positive integer, got %q", v)
		}
		cfg.SessionCycleLimit = n
	}

	if v := os.Getenv("ADMIN_IDS"); v != "" {
		ids, err := parseAdminIDs(v)
		if err != nil {
			return nil, err
		}
		cfg.AdminIDs = ids
	}

	return cfg, nil
}

// parseAdminIDs parses a comma-separated list of Telegram user IDs,
// e.g. "123456789, 987654321". Blank entries between commas are
// ignored so a trailing comma doesn't error out.
func parseAdminIDs(v string) ([]int64, error) {
	parts := strings.Split(v, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ADMIN_IDS contains invalid telegram id %q: %w", p, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
