package llm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/openai/openai-go"

	"sa-hr-bot/internal/db"
)

// sessionCycleLimitPlaceholder is the token in prompts/system_prompt.md
// that gets substituted with the configured session cycle limit once,
// at Service construction time.
const sessionCycleLimitPlaceholder = "{{SESSION_CYCLE_LIMIT}}"

// maxRecentTurns bounds how much dialog history is folded into each
// call's user message. See BuildUserContext for why this stays small.
const maxRecentTurns = 2

// QuestionPicker is the subset of db.Repository this package depends on.
// Defined here, rather than depending on the full repository, so tests
// can substitute a fake without a real database.
type QuestionPicker interface {
	PickQuestion(ctx context.Context, grade, topic string) (*db.QuestionBank, error)
}

// StudentProfile is the compact learner profile folded into every user
// message: the target grade, never the full qualification transcript.
type StudentProfile struct {
	Grade string // target grade, e.g. "джун", "джун-мидл", "мидл", "мидл-сеньор", "сеньор"
}

// Turn is one message in the recent dialog history.
type Turn struct {
	Role    string // "user" or "assistant"
	Content string
}

// Usage mirrors the token accounting OpenAI returns for a completion,
// including the automatic prompt-cache hit count when the API reports it.
type Usage struct {
	PromptTokens      int64
	CompletionTokens  int64
	TotalTokens       int64
	CachedTokens      int64
	CachedTokensKnown bool // whether the response actually included cached_tokens
}

// Reply is the model's answer together with the usage of the call that
// produced it.
type Reply struct {
	Text  string
	Usage Usage
}

// Service is the entry point for talking to the interview LLM: it keeps
// the system prompt stable across calls (for OpenAI's automatic prompt
// caching) and assembles a compact per-call user context instead of
// replaying the full conversation.
type Service struct {
	client       *Client
	questions    QuestionPicker
	logger       *slog.Logger
	systemPrompt string
}

// ServiceConfig configures NewService.
type ServiceConfig struct {
	// PromptPath is the path to the system prompt markdown file, e.g.
	// "prompts/system_prompt.md" (relative to the process working
	// directory, which is how the Dockerfile and local `go run` both
	// resolve it).
	PromptPath string

	// SessionCycleLimit is substituted into every occurrence of
	// {{SESSION_CYCLE_LIMIT}} in the prompt file.
	SessionCycleLimit int
}

// NewService loads and prepares the system prompt once, so the exact
// same prompt text is reused for every call across a session. OpenAI's
// automatic prefix caching keys off the request being byte-identical up
// to the point where it diverges, so the prompt must never be
// regenerated or tweaked per-call: only the per-call user message
// changes.
func NewService(client *Client, questions QuestionPicker, logger *slog.Logger, cfg ServiceConfig) (*Service, error) {
	raw, err := os.ReadFile(cfg.PromptPath)
	if err != nil {
		return nil, fmt.Errorf("read system prompt %s: %w", cfg.PromptPath, err)
	}

	prompt := strings.ReplaceAll(string(raw), sessionCycleLimitPlaceholder, strconv.Itoa(cfg.SessionCycleLimit))

	return &Service{
		client:       client,
		questions:    questions,
		logger:       logger,
		systemPrompt: prompt,
	}, nil
}

// PickQuestion selects an interview question matching the candidate's
// grade and (optionally) topic. Call it before Reply during phase 3 of
// the system prompt (question cycle) and pass the result into
// BuildUserContext so the model has the question text and reference
// answers to work with.
func (s *Service) PickQuestion(ctx context.Context, grade, topic string) (*db.QuestionBank, error) {
	return s.questions.PickQuestion(ctx, grade, topic)
}

// BuildUserContext assembles the compact, per-call user message: the
// candidate's profile, the current weak-zone map, at most the last two
// dialog turns, and, when the caller is mid question-cycle, the picked
// bank question with its follow-ups and reference answers.
//
// The full conversation history is deliberately left out. Replaying it
// every call would make prompt_tokens grow without bound over an
// 8-cycle session, and, more importantly, would change the request's
// content on every call. OpenAI's automatic caching discounts the
// prefix shared with a prior request; a stable system prompt plus a
// short, bounded user message keeps that prefix (and the discount)
// intact for the whole session.
func BuildUserContext(profile StudentProfile, weakZones []db.WeakZone, recentTurns []Turn, question *db.QuestionBank) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Профиль кандидата:\nЦелевой грейд: %s\n", profile.Grade)

	b.WriteString("\nКарта слабых зон:\n")
	if len(weakZones) == 0 {
		b.WriteString("(пока пусто)\n")
	} else {
		for _, wz := range weakZones {
			fmt.Fprintf(&b, "- %s: %s\n", wz.ZoneText, wz.Status)
		}
	}

	if question != nil {
		fmt.Fprintf(&b, "\nВопрос из банка (id %d, грейд %s, тема %s):\n%s\n",
			question.ID, question.Grade, question.Topic, question.QuestionText)
		fmt.Fprintf(&b, "Follow-up 1: %s\nFollow-up 2: %s\n", question.Followup1, question.Followup2)
		fmt.Fprintf(&b, "Эталон джун: %s\nЭталон мидл: %s\nЭталон сеньор: %s\n",
			question.AnswerJunior, question.AnswerMiddle, question.AnswerSenior)
	}

	turns := recentTurns
	if len(turns) > maxRecentTurns {
		turns = turns[len(turns)-maxRecentTurns:]
	}

	b.WriteString("\nПоследние реплики диалога:\n")
	if len(turns) == 0 {
		b.WriteString("(диалог только начинается)\n")
	} else {
		for _, t := range turns {
			fmt.Fprintf(&b, "%s: %s\n", t.Role, t.Content)
		}
	}

	return b.String()
}

// QualificationKnown is what has already been extracted for the current
// session's four QUALIFICATION fields (see prompts/system_prompt.md's
// "Технические метки" section). An empty field means it is still
// unknown.
type QualificationKnown struct {
	CurrentGrade   string
	TargetGrade    string
	StudentRequest string
	SelfAssessment string
}

// qualificationFieldLabels pairs each QualificationKnown field with the
// label used to render it, in the fixed order the "known"/"missing"
// lists are always presented in.
var qualificationFieldLabels = []struct {
	label string
	get   func(QualificationKnown) string
}{
	{"Текущий грейд", func(k QualificationKnown) string { return k.CurrentGrade }},
	{"Целевой грейд", func(k QualificationKnown) string { return k.TargetGrade }},
	{"Запрос", func(k QualificationKnown) string { return k.StudentRequest }},
	{"Самооценка сильных/слабых зон", func(k QualificationKnown) string { return k.SelfAssessment }},
}

// BuildQualificationContext assembles the compact per-call context for
// phase 1 (QUALIFICATION). Unlike BuildUserContext, it also spells out
// exactly which of the four qualification fields are already known
// (with their values) and which are still missing, and instructs the
// model not to re-ask what it already has or repeat the phase 0
// instruction.
//
// This is necessary, not cosmetic: the compact per-call context (see
// BuildUserContext's doc comment) never carries the full conversation,
// so a model with no memory of earlier turns has no other way to know
// it already asked about, say, the current grade two turns ago. Without
// this explicit list the model tends to re-ask questions it already got
// answers to.
func BuildQualificationContext(known QualificationKnown, weakZones []db.WeakZone, studentAnswer string) string {
	var b strings.Builder

	b.WriteString("Инструкция уже была показана один раз в Фазе 0, не повторяй ее.\n\n")

	b.WriteString("Уже известно:\n")
	anyKnown := false
	for _, f := range qualificationFieldLabels {
		if v := f.get(known); v != "" {
			fmt.Fprintf(&b, "- %s: %s\n", f.label, v)
			anyKnown = true
		}
	}
	if !anyKnown {
		b.WriteString("(пока ничего не известно)\n")
	}

	b.WriteString("\nЕще не известно:\n")
	anyMissing := false
	for _, f := range qualificationFieldLabels {
		if f.get(known) == "" {
			fmt.Fprintf(&b, "- %s\n", f.label)
			anyMissing = true
		}
	}
	if !anyMissing {
		b.WriteString("(все четыре пункта уже известны)\n")
	}

	b.WriteString("\nСпроси только про недостающее, по одному вопросу за раз.\n")

	b.WriteString("\nКарта слабых зон:\n")
	if len(weakZones) == 0 {
		b.WriteString("(пока пусто)\n")
	} else {
		for _, wz := range weakZones {
			fmt.Fprintf(&b, "- %s: %s\n", wz.ZoneText, wz.Status)
		}
	}

	b.WriteString("\nПоследний ответ кандидата:\n")
	fmt.Fprintf(&b, "%s\n", studentAnswer)

	return b.String()
}

// Evaluate grades studentAnswer against question's reference answers
// (answer_junior/middle/senior) and follow-ups, folded into context via
// BuildUserContext exactly like any other turn. Call it whenever the
// incoming message is a response to a specific, already-known bank
// question (sessions.current_question_id is set) rather than relying on
// the model's own memory of which question it asked: the compact
// per-call context (see BuildUserContext) does not carry prior turns
// far enough back for that to be reliable.
func (s *Service) Evaluate(ctx context.Context, profile StudentProfile, weakZones []db.WeakZone, studentAnswer string, question *db.QuestionBank) (*Reply, error) {
	turns := []Turn{{Role: "user", Content: studentAnswer}}
	return s.Reply(ctx, BuildUserContext(profile, weakZones, turns, question))
}

// Reply sends the stable system prompt together with userMessage to the
// model and returns its answer. It logs token usage on every call so
// real spend, and the savings from prompt caching, can be tracked.
func (s *Service) Reply(ctx context.Context, userMessage string) (*Reply, error) {
	resp, err := s.client.api.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: s.client.model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(s.systemPrompt),
			openai.UserMessage(userMessage),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create chat completion: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("chat completion returned no choices")
	}

	u := resp.Usage
	cachedKnown := u.PromptTokensDetails.JSON.CachedTokens.Valid()

	logArgs := []any{
		"model", resp.Model,
		"prompt_tokens", u.PromptTokens,
		"completion_tokens", u.CompletionTokens,
		"total_tokens", u.TotalTokens,
	}
	if cachedKnown {
		logArgs = append(logArgs, "cached_tokens", u.PromptTokensDetails.CachedTokens)
	}
	s.logger.Info("openai chat completion usage", logArgs...)

	return &Reply{
		Text: resp.Choices[0].Message.Content,
		Usage: Usage{
			PromptTokens:      u.PromptTokens,
			CompletionTokens:  u.CompletionTokens,
			TotalTokens:       u.TotalTokens,
			CachedTokens:      u.PromptTokensDetails.CachedTokens,
			CachedTokensKnown: cachedKnown,
		},
	}, nil
}
