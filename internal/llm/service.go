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

type sessionQuestionPicker interface {
	PickQuestionForSession(ctx context.Context, sessionID int64, grade, topic string) (*db.QuestionBank, error)
}

// StudentProfile is the compact learner profile folded into every evaluation
// request. Grade is retained as a compatibility alias for TargetGrade while
// older sessions/tests are migrated.
type StudentProfile struct {
	Grade        string
	CurrentGrade string
	TargetGrade  string
	StrongZones  string
	WeakZones    string
}

// QualificationProfile contains the deterministic onboarding answers used by
// summary. Legacy fields remain available for an older active session.
type QualificationProfile struct {
	CurrentGrade string
	TargetGrade  string
	StrongZones  string
	WeakZones    string

	// Legacy pre-stakeholder-onboarding fields.
	Grade           string
	Direction       string
	Experience      string
	InterviewTarget string
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

// PickQuestionForSession uses session-aware exclusion when the repository
// supports it and falls back to the legacy picker in isolated tests.
func (s *Service) PickQuestionForSession(ctx context.Context, sessionID int64, grade, topic string) (*db.QuestionBank, error) {
	if picker, ok := s.questions.(sessionQuestionPicker); ok {
		return picker.PickQuestionForSession(ctx, sessionID, grade, topic)
	}
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

	targetGrade := profile.TargetGrade
	if targetGrade == "" {
		targetGrade = profile.Grade
	}
	fmt.Fprintf(&b, "Профиль кандидата:\nТекущий грейд: %s\nЦелевой грейд: %s\n", profile.CurrentGrade, targetGrade)
	fmt.Fprintf(&b, "Сильные зоны по самооценке: %s\nСлабые зоны по самооценке: %s\n", profile.StrongZones, profile.WeakZones)

	b.WriteString("\nКарта слабых зон:\n")
	if len(weakZones) == 0 {
		b.WriteString("(пока пусто)\n")
	} else {
		for _, wz := range weakZones {
			fmt.Fprintf(&b, "- %s: %s\n", wz.ZoneText, wz.Status)
		}
	}

	if question != nil {
		fmt.Fprintf(&b, "\nВопрос из банка (id %d, грейд %s, тема %s):\nКонтекст: %s\n%s\n",
			question.ID, question.Grade, question.Topic, question.QuestionContext, question.QuestionText)
		fmt.Fprintf(&b, "Follow-up 1 (%s): %s\nFollow-up 2 (%s): %s\n",
			question.Followup1Context, question.Followup1, question.Followup2Context, question.Followup2)
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

// BuildAuditContext explicitly tells the model to run phase 2 and supplies all
// qualification answers. The phase must be explicit because the service does
// not replay the full Telegram conversation on every call.
func BuildAuditContext(profile QualificationProfile, weakZones []db.WeakZone) string {
	var b strings.Builder

	b.WriteString("Текущая задача: выполни Фазу 2, МИНИ-АУДИТ. Инструкцию и вопросы квалификации не повторяй.\n\n")
	b.WriteString("Ответы квалификации:\n")
	fmt.Fprintf(&b, "- Грейд: %s\n", profile.Grade)
	fmt.Fprintf(&b, "- Направление и индустрия: %s\n", profile.Direction)
	fmt.Fprintf(&b, "- Реальный опыт и пробелы: %s\n", profile.Experience)
	fmt.Fprintf(&b, "- Вакансия или дата собеседования: %s\n", profile.InterviewTarget)

	b.WriteString("\nТекущая карта слабых зон:\n")
	if len(weakZones) == 0 {
		b.WriteString("(пока пусто)\n")
	} else {
		for _, wz := range weakZones {
			fmt.Fprintf(&b, "- %s: %s\n", wz.ZoneText, wz.Status)
		}
	}

	b.WriteString("\nСформулируй 2-4 гипотезы и оформи их в рамке МИНИ-АУДИТ. В конце добавь WEAK_TOPICS по правилам системного промпта.\n")
	return b.String()
}

// BuildEvaluationContext pins the model to phase 3. Without this explicit
// instruction, a short or nonsensical candidate answer can make the model
// incorrectly restart phase 0 or qualification from the large system prompt.
func BuildEvaluationContext(profile StudentProfile, weakZones []db.WeakZone, studentAnswer string, question *db.QuestionBank) string {
	var b strings.Builder
	b.WriteString("Текущая задача: ФАЗА 3, оцени ответ кандидата на уже заданный технический вопрос.\n")
	b.WriteString("В этом сообщении говори в роли интервьюера, который проводит техническое собеседование.\n")
	b.WriteString("Не повторяй инструкцию. Не начинай квалификацию. Не спрашивай грейд, направление, опыт или дату собеседования.\n")
	b.WriteString("Даже если ответ бессмысленный, грубый или не относится к вопросу, оставайся в Фазе 3. Прямо скажи, что ответ не раскрывает тему, и кратко объясни, чего не хватило.\n")
	b.WriteString("Верни только одну рамку `▸ ОБРАТНАЯ СВЯЗЬ`. Не задавай следующий вопрос и не начинай новую фазу: уточнение из банка вопросов отправит код.\n\n")
	b.WriteString(BuildUserContext(profile, weakZones, []Turn{{Role: "user", Content: studentAnswer}}, question))
	return b.String()
}

// Evaluate grades studentAnswer against question's reference answers while
// explicitly keeping the model in phase 3.
func (s *Service) Evaluate(ctx context.Context, profile StudentProfile, weakZones []db.WeakZone, studentAnswer string, question *db.QuestionBank) (*Reply, error) {
	return s.Reply(ctx, BuildEvaluationContext(profile, weakZones, studentAnswer, question))
}

// BuildFollowupEvaluationContext asks only for the mini-feedback after the
// first follow-up. The code sends the bank's second follow-up afterwards.
func BuildFollowupEvaluationContext(profile StudentProfile, weakZones []db.WeakZone, primaryAnswer, followupQuestion, followupAnswer string, question *db.QuestionBank) string {
	var b strings.Builder
	b.WriteString("Текущая задача: ФАЗА 3. Дай короткую обратную связь только на ответ кандидата на первый уточняющий вопрос.\n")
	b.WriteString("В этом сообщении говори в роли интервьюера, который проводит техническое собеседование.\n")
	b.WriteString("Не повторяй инструкцию и квалификацию. Не спрашивай грейд. Не задавай новый вопрос и не давай большой разбор блока.\n")
	b.WriteString("Верни только одну рамку `▸ ОБРАТНАЯ СВЯЗЬ`: конкретно укажи, что ответ раскрыл и чего в нем не хватило.\n")
	b.WriteString("Даже если один из ответов грубый или бессмысленный, оставайся в этой задаче и оцени отсутствие содержательного ответа прямо.\n\n")
	b.WriteString(BuildUserContext(profile, weakZones, nil, question))
	fmt.Fprintf(&b, "\nОсновной ответ кандидата:\n%s\n", primaryAnswer)
	fmt.Fprintf(&b, "\nУточняющий вопрос:\n%s\n", followupQuestion)
	fmt.Fprintf(&b, "\nОтвет кандидата на уточнение:\n%s\n", followupAnswer)
	return b.String()
}

// EvaluateFollowup produces mini-feedback after the second answer.
func (s *Service) EvaluateFollowup(ctx context.Context, profile StudentProfile, weakZones []db.WeakZone, primaryAnswer, followupQuestion, followupAnswer string, question *db.QuestionBank) (*Reply, error) {
	return s.Reply(ctx, BuildFollowupEvaluationContext(
		profile, weakZones, primaryAnswer, followupQuestion, followupAnswer, question,
	))
}

// BuildBlockEvaluationContext asks for the mini-feedback on answer three and
// then one evidence-based review of all three answers using the approved KDIR
// structure. A service marker updates the weak-zone map but is hidden from the
// candidate by the handler.
func BuildBlockEvaluationContext(profile StudentProfile, weakZones []db.WeakZone, primaryAnswer, followup1Question, followup1Answer, followup2Question, followup2Answer string, question *db.QuestionBank) string {
	var b strings.Builder
	b.WriteString("Текущая задача: ФАЗА 3. Кандидат ответил на основной вопрос и два уточнения. Заверши блок.\n")
	b.WriteString("В этом сообщении говори в роли интервьюера, который проводит техническое собеседование.\n")
	b.WriteString("Не повторяй инструкцию и квалификацию. Не спрашивай грейд и не задавай новый вопрос.\n")
	b.WriteString("Сначала дай одну короткую рамку `▸ ОБРАТНАЯ СВЯЗЬ` только по ответу на второе уточнение.\n")
	b.WriteString("Затем дай отдельную рамку `▸ БОЛЬШАЯ ОБРАТНАЯ СВЯЗЬ` по всей связке из трех ответов. Обязательно используй формулу КДИР: Контекст, Действие, Инструмент, Результат. Для каждого пункта укажи, что было и чего не хватило.\n")
	b.WriteString("В конце большой рамки добавь короткий пример более сильного ответа, опираясь только на вопрос, эталоны и факты кандидата. Не приписывай кандидату выдуманный опыт.\n")
	b.WriteString("В самом конце добавь служебную строку `WEAK_ZONE_STATUS: confirmed`, если пробел по теме остался, или `WEAK_ZONE_STATUS: closed`, если кандидат его закрыл. Других служебных строк не добавляй.\n")
	b.WriteString("Даже если ответ грубый, бессмысленный или не относится к вопросу, не меняй фазу и прямо оцени отсутствие содержательного ответа.\n\n")
	b.WriteString(BuildUserContext(profile, weakZones, nil, question))
	fmt.Fprintf(&b, "\nОсновной ответ кандидата:\n%s\n", primaryAnswer)
	fmt.Fprintf(&b, "\nПервый уточняющий вопрос:\n%s\nОтвет:\n%s\n", followup1Question, followup1Answer)
	fmt.Fprintf(&b, "\nВторой уточняющий вопрос:\n%s\nОтвет:\n%s\n", followup2Question, followup2Answer)
	return b.String()
}

func (s *Service) EvaluateBlock(ctx context.Context, profile StudentProfile, weakZones []db.WeakZone, primaryAnswer, followup1Question, followup1Answer, followup2Question, followup2Answer string, question *db.QuestionBank) (*Reply, error) {
	return s.Reply(ctx, BuildBlockEvaluationContext(
		profile, weakZones, primaryAnswer, followup1Question, followup1Answer,
		followup2Question, followup2Answer, question,
	))
}

// BuildSummaryContext creates an explicit phase-4 request from facts persisted
// across the completed question attempts.
func BuildSummaryContext(profile QualificationProfile, weakZones []db.WeakZone, attempts []db.QuestionAttemptReport) string {
	var b strings.Builder
	b.WriteString("Текущая задача: ФАЗА 4, итоговый отчет по завершенной сессии.\n")
	b.WriteString("Не повторяй инструкцию, квалификацию или вопросы. Используй только факты ниже.\n")
	b.WriteString("Дай краткую итоговую карту слабых зон и 2-3 конкретные рекомендации перед собеседованием.\n\n")
	if profile.TargetGrade != "" || profile.CurrentGrade != "" || profile.StrongZones != "" || profile.WeakZones != "" {
		fmt.Fprintf(&b, "Текущий грейд: %s\n", profile.CurrentGrade)
		fmt.Fprintf(&b, "Целевой грейд: %s\n", profile.TargetGrade)
		fmt.Fprintf(&b, "Сильные зоны по самооценке: %s\n", profile.StrongZones)
		fmt.Fprintf(&b, "Слабые зоны по самооценке: %s\n", profile.WeakZones)
	} else {
		fmt.Fprintf(&b, "Грейд кандидата: %s\n", profile.Grade)
		fmt.Fprintf(&b, "Направление и индустрия: %s\n", profile.Direction)
		fmt.Fprintf(&b, "Исходный опыт и пробелы: %s\n", profile.Experience)
		fmt.Fprintf(&b, "Вакансия или дата собеседования: %s\n", profile.InterviewTarget)
	}

	b.WriteString("\nКарта слабых зон:\n")
	if len(weakZones) == 0 {
		b.WriteString("(нет сохраненных зон)\n")
	} else {
		for _, zone := range weakZones {
			fmt.Fprintf(&b, "- %s: %s\n", zone.ZoneText, zone.Status)
		}
	}

	b.WriteString("\nЗавершенные циклы:\n")
	if len(attempts) == 0 {
		b.WriteString("(нет сохраненных циклов)\n")
	} else {
		for i, attempt := range attempts {
			fmt.Fprintf(&b, "\nЦикл %d, тема %s\n", i+1, attempt.Topic)
			fmt.Fprintf(&b, "Основной вопрос: %s\n", attempt.QuestionText)
			fmt.Fprintf(&b, "Основной ответ: %s\n", attempt.PrimaryAnswer)
			fmt.Fprintf(&b, "Уточнение: %s\n", attempt.FollowupQuestion)
			fmt.Fprintf(&b, "Ответ на уточнение: %s\n", attempt.FollowupAnswer)
			fmt.Fprintf(&b, "Второе уточнение: %s\n", attempt.Followup2Question)
			fmt.Fprintf(&b, "Ответ на второе уточнение: %s\n", attempt.Followup2Answer)
			fmt.Fprintf(&b, "Зафиксированная обратная связь: %s\n", attempt.FinalFeedback)
		}
	}
	return b.String()
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
