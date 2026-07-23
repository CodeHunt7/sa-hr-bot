// Package llm wraps the official OpenAI Go SDK so the rest of the
// application depends on a small interface instead of the SDK directly.
package llm

import (
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

const defaultBaseURL = "https://api.openai.com/v1/"

// defaultModel is used when Config.Model is empty.
//
// Why gpt-4.1-mini for this bot:
//   - It is priced in the same economy tier as gpt-4o-mini, far cheaper
//     than gpt-4.1/gpt-4o or any reasoning (o-series) model, which matters
//     for a course tool students will hit repeatedly across long sessions.
//   - Its instruction-following is noticeably better than gpt-4o-mini's,
//     and this bot leans on that: the system prompt enforces a strict,
//     unusual style contract (no "ё", no mid-sentence dashes, banned
//     filler phrases, rigid framed output blocks). A model that drifts on
//     formatting rules breaks the persona every few turns.
//   - It is not a reasoning model, so it responds with low latency and
//     without the extra reasoning-token cost that o3-mini/o4-mini would
//     add for what is fundamentally short-form conversational grading,
//     not multi-step math/code reasoning.
//   - Its large context window comfortably holds the stable system prompt
//     plus the compact per-call user context, which keeps the cached
//     prompt prefix (see service.go) well within limits over a full
//     8-cycle interview session.
const defaultModel = openai.ChatModelGPT4_1Mini

// Client wraps the OpenAI SDK client with the configuration needed for
// this bot (API key, optional proxy base URL, default model).
type Client struct {
	api   openai.Client
	model string
}

// Config holds the settings required to build a Client.
type Config struct {
	APIKey  string
	BaseURL string // optional, e.g. for routing requests through a proxy or a self-hosted gateway
	Model   string // optional, e.g. "gpt-4.1-mini"; empty uses defaultModel
}

// New creates an OpenAI client. The base URL is always passed explicitly:
// openai-go treats an existing but empty OPENAI_BASE_URL environment variable
// as an override and otherwise builds invalid relative URLs such as
// "/chat/completions". Docker Compose commonly creates exactly that empty
// variable when an optional value is omitted.
func New(cfg Config) *Client {
	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithBaseURL(resolveBaseURL(cfg.BaseURL)),
	}

	model := cfg.Model
	if model == "" {
		model = defaultModel
	}

	return &Client{
		api:   openai.NewClient(opts...),
		model: model,
	}
}

func resolveBaseURL(configured string) string {
	if baseURL := strings.TrimSpace(configured); baseURL != "" {
		return baseURL
	}
	return defaultBaseURL
}
