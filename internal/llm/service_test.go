package llm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sa-hr-bot/internal/db"
)

type fakeQuestionPicker struct {
	question *db.QuestionBank
	err      error

	gotGrade, gotTopic string
}

func (f *fakeQuestionPicker) PickQuestion(_ context.Context, grade, topic string) (*db.QuestionBank, error) {
	f.gotGrade, f.gotTopic = grade, topic
	return f.question, f.err
}

// newTestService points a Service at a local stub server instead of the
// real OpenAI API, so Reply's request/response handling can be exercised
// without network access or a paid API call.
func newTestService(t *testing.T, handler http.HandlerFunc, picker QuestionPicker) *Service {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client := New(Config{
		APIKey:  "test-key",
		BaseURL: srv.URL + "/v1/",
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	svc, err := NewService(client, picker, logger, ServiceConfig{
		PromptPath:        "../../prompts/system_prompt.md",
		SessionCycleLimit: 8,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestNewService_SubstitutesSessionCycleLimit(t *testing.T) {
	svc := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no HTTP call expected")
	}, &fakeQuestionPicker{})

	if strings.Contains(svc.systemPrompt, sessionCycleLimitPlaceholder) {
		t.Fatalf("systemPrompt still contains unresolved placeholder: %q", svc.systemPrompt)
	}
	if !strings.Contains(svc.systemPrompt, "лимит в 8 циклов") {
		t.Fatalf("systemPrompt does not contain substituted limit: %q", svc.systemPrompt)
	}
}

func TestReply_UsesCustomBaseURLAndParsesUsage(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody map[string]any

	svc := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-test",
			"object": "chat.completion",
			"created": 1,
			"model": "gpt-4.1-mini",
			"choices": [{
				"index": 0,
				"finish_reason": "stop",
				"message": {"role": "assistant", "content": "твой уровень мидл )"}
			}],
			"usage": {
				"prompt_tokens": 120,
				"completion_tokens": 30,
				"total_tokens": 150,
				"prompt_tokens_details": {"cached_tokens": 96}
			}
		}`))
	}, &fakeQuestionPicker{})

	reply, err := svc.Reply(context.Background(), "Профиль кандидата:\nГрейд: джун\n")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	if !strings.Contains(gotPath, "chat/completions") {
		t.Fatalf("unexpected request path: %q", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("unexpected Authorization header: %q", gotAuth)
	}

	messages, _ := gotBody["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages (system, user), got %d: %v", len(messages), gotBody["messages"])
	}
	sysMsg := messages[0].(map[string]any)
	if sysMsg["role"] != "system" || sysMsg["content"] != svc.systemPrompt {
		t.Fatalf("system message mismatch: %v", sysMsg)
	}

	if reply.Text != "твой уровень мидл )" {
		t.Fatalf("unexpected reply text: %q", reply.Text)
	}
	if reply.Usage.PromptTokens != 120 || reply.Usage.CompletionTokens != 30 || reply.Usage.TotalTokens != 150 {
		t.Fatalf("unexpected usage: %+v", reply.Usage)
	}
	if !reply.Usage.CachedTokensKnown || reply.Usage.CachedTokens != 96 {
		t.Fatalf("expected cached_tokens=96 to be recognized, got %+v", reply.Usage)
	}
}

func TestReply_CachedTokensAbsentWhenNotReported(t *testing.T) {
	svc := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-test",
			"object": "chat.completion",
			"created": 1,
			"model": "gpt-4.1-mini",
			"choices": [{
				"index": 0,
				"finish_reason": "stop",
				"message": {"role": "assistant", "content": "ok"}
			}],
			"usage": {
				"prompt_tokens": 50,
				"completion_tokens": 5,
				"total_tokens": 55
			}
		}`))
	}, &fakeQuestionPicker{})

	reply, err := svc.Reply(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply.Usage.CachedTokensKnown {
		t.Fatalf("expected CachedTokensKnown=false when cached_tokens is absent, got %+v", reply.Usage)
	}
}

func TestPickQuestion_DelegatesToRepository(t *testing.T) {
	want := &db.QuestionBank{ID: 1, QuestionText: "Что такое 3НФ?", Grade: "джун", Topic: "бд"}
	picker := &fakeQuestionPicker{question: want}

	svc := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("no HTTP call expected")
	}, picker)

	got, err := svc.PickQuestion(context.Background(), "джун", "бд")
	if err != nil {
		t.Fatalf("PickQuestion: %v", err)
	}
	if got != want {
		t.Fatalf("expected picker's question to be returned as-is, got %+v", got)
	}
	if picker.gotGrade != "джун" || picker.gotTopic != "бд" {
		t.Fatalf("grade/topic not forwarded correctly: %q/%q", picker.gotGrade, picker.gotTopic)
	}
}

func TestBuildUserContext(t *testing.T) {
	profile := StudentProfile{Grade: "джун"}
	weakZones := []db.WeakZone{
		{ZoneText: "интеграции", Status: db.WeakZoneStatusHypothesis},
		{ZoneText: "требования", Status: db.WeakZoneStatusConfirmed},
	}
	turns := []Turn{
		{Role: "assistant", Content: "первая реплика, должна быть обрезана"},
		{Role: "user", Content: "предпоследняя реплика"},
		{Role: "assistant", Content: "последняя реплика"},
	}
	question := &db.QuestionBank{
		ID: 42, Grade: "джун", Topic: "бд", QuestionText: "Что такое нормализация?",
		Followup1: "А что такое 3НФ?", Followup2: "Приведи пример денормализации",
		AnswerJunior: "джун ответ", AnswerMiddle: "мидл ответ", AnswerSenior: "сеньор ответ",
	}

	got := BuildUserContext(profile, weakZones, turns, question)

	for _, want := range []string{
		"Целевой грейд: джун",
		"интеграции: hypothesis",
		"требования: confirmed",
		"Что такое нормализация?",
		"Follow-up 1: А что такое 3НФ?",
		"Эталон сеньор: сеньор ответ",
		"предпоследняя реплика",
		"последняя реплика",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected context to contain %q, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "первая реплика, должна быть обрезана") {
		t.Errorf("expected only the last %d turns to be included, got:\n%s", maxRecentTurns, got)
	}
}

func TestBuildQualificationContext_PartiallyKnown(t *testing.T) {
	known := QualificationKnown{
		CurrentGrade: "джун",
		TargetGrade:  "мидл",
		// StudentRequest and SelfAssessment intentionally left unknown.
	}

	got := BuildQualificationContext(known, nil, "хочу подготовиться к собеседованию в Т-Банк")

	for _, want := range []string{
		"Инструкция уже была показана один раз в Фазе 0, не повторяй ее.",
		"Текущий грейд: джун",
		"Целевой грейд: мидл",
		"Спроси только про недостающее",
		"хочу подготовиться к собеседованию в Т-Банк",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected context to contain %q, got:\n%s", want, got)
		}
	}

	// The two known fields must be listed as known, not as missing.
	knownSection := got[:strings.Index(got, "Еще не известно:")]
	missingSection := got[strings.Index(got, "Еще не известно:"):]
	if strings.Contains(missingSection, "Текущий грейд") || strings.Contains(missingSection, "Целевой грейд") {
		t.Errorf("known fields leaked into the missing section:\n%s", missingSection)
	}
	if !strings.Contains(missingSection, "Запрос") || !strings.Contains(missingSection, "Самооценка") {
		t.Errorf("expected Запрос and Самооценка to be listed as missing:\n%s", missingSection)
	}
	if strings.Contains(knownSection, "Запрос:") || strings.Contains(knownSection, "Самооценка") {
		t.Errorf("unknown fields leaked into the known section:\n%s", knownSection)
	}
}

func TestBuildQualificationContext_AllKnown(t *testing.T) {
	known := QualificationKnown{
		CurrentGrade:   "джун",
		TargetGrade:    "мидл",
		StudentRequest: "хочет подготовиться к смене грейда",
		SelfAssessment: "хорошо знает БД, слабо в архитектуре",
	}

	got := BuildQualificationContext(known, nil, "да, все верно")

	if !strings.Contains(got, "(все четыре пункта уже известны)") {
		t.Errorf("expected the missing section to say all four are known, got:\n%s", got)
	}
}

func TestEvaluate_SendsQuestionAndReferenceAnswersAsContext(t *testing.T) {
	var gotUserContent string

	svc := newTestService(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		messages, _ := body["messages"].([]any)
		if len(messages) != 2 {
			t.Fatalf("expected 2 messages (system, user), got %d", len(messages))
		}
		userMsg := messages[1].(map[string]any)
		gotUserContent, _ = userMsg["content"].(string)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-test", "object": "chat.completion", "created": 1, "model": "gpt-4.1-mini",
			"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "оценка"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}
		}`))
	}, &fakeQuestionPicker{})

	question := &db.QuestionBank{
		ID: 7, Grade: "мидл", Topic: "бд", QuestionText: "Что такое индекс в БД?",
		Followup1: "Какие виды индексов бывают?", Followup2: "Когда индекс вредит?",
		AnswerJunior: "junior-ref", AnswerMiddle: "middle-ref", AnswerSenior: "senior-ref",
	}
	profile := StudentProfile{Grade: "мидл"}
	studentAnswer := "индекс ускоряет чтение, но замедляет запись"

	reply, err := svc.Evaluate(context.Background(), profile, nil, studentAnswer, question)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if reply.Text != "оценка" {
		t.Fatalf("unexpected reply text: %q", reply.Text)
	}

	for _, want := range []string{studentAnswer, "junior-ref", "middle-ref", "senior-ref", "Что такое индекс в БД?"} {
		if !strings.Contains(gotUserContent, want) {
			t.Errorf("expected the request's user message to contain %q, got:\n%s", want, gotUserContent)
		}
	}
}
