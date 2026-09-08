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
	if !strings.Contains(svc.systemPrompt, "8 завершенных блоков") {
		t.Fatalf("systemPrompt does not contain substituted limit: %q", svc.systemPrompt)
	}
	for _, want := range []string{
		"Ты живой интервьюер и наставник",
		"Обращение к кандидату только на \"вы\"",
		"вы указали",
		"не единственно правильными",
		"КДИР является рекомендацией",
		"Существенного пробела по этому вопросу нет",
		"Не выдумывай недостаток ради формата",
	} {
		if !strings.Contains(svc.systemPrompt, want) {
			t.Fatalf("systemPrompt does not contain soft evaluation rule %q", want)
		}
	}
	if strings.Contains(svc.systemPrompt, "Прямота без смягчения") {
		t.Fatal("systemPrompt still contains the old harsh tone rule")
	}
	if strings.Contains(svc.systemPrompt, "есть в ответе, один главный пробел") {
		t.Fatal("systemPrompt still requires a gap in every answer")
	}
	if strings.Contains(svc.systemPrompt, "Говори на \"ты\"") || strings.Contains(svc.systemPrompt, "Обращение на \"ты\"") {
		t.Fatal("systemPrompt still instructs the model to address the candidate informally")
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
	if gotBody["temperature"] != 0.2 {
		t.Fatalf("temperature = %v, want 0.2 for stable evaluation", gotBody["temperature"])
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
		ID: 42, Grade: "джун", Topic: "бд", QuestionText: "Что такое нормализация?", QuestionContext: "Контекст БД",
		Followup1: "А что такое 3НФ?", Followup1Context: "Контекст уточнения 1",
		Followup2: "Приведи пример денормализации", Followup2Context: "Контекст уточнения 2",
		AnswerJunior: "джун ответ", AnswerMiddle: "мидл ответ", AnswerSenior: "сеньор ответ",
	}

	got := BuildUserContext(profile, weakZones, turns, question)

	for _, want := range []string{
		"Целевой грейд: джун",
		"интеграции: hypothesis",
		"требования: confirmed",
		"Что такое нормализация?",
		"Follow-up 1 (Контекст уточнения 1): А что такое 3НФ?",
		"Пример хорошего ответа для ориентира, не единственно правильный вариант: джун ответ",
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
	if strings.Contains(got, "мидл ответ") || strings.Contains(got, "сеньор ответ") {
		t.Errorf("expected only the target-grade reference answer, got:\n%s", got)
	}
}

func TestBuildAuditContext_IncludesFullQualificationProfileAndExplicitPhase(t *testing.T) {
	profile := QualificationProfile{
		Grade:           "мидл",
		Direction:       "финтех",
		Experience:      "есть требования, мало Kafka",
		InterviewTarget: "собеседование 20 июля",
	}
	zones := []db.WeakZone{{ZoneText: "интеграции", Status: db.WeakZoneStatusHypothesis}}

	got := BuildAuditContext(profile, zones)
	for _, want := range []string{
		"Фазу 2", "мидл", "финтех", "есть требования, мало Kafka",
		"собеседование 20 июля", "интеграции: hypothesis", "WEAK_TOPICS",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("audit context does not contain %q:\n%s", want, got)
		}
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

	for _, want := range []string{
		studentAnswer, "middle-ref", "Что такое индекс в БД?",
		"ФАЗА 3", "Не начинай квалификацию", "Не спрашивай грейд", "ответ бессмысленный",
		"живой интервьюер и наставник", "не единственно правильным вариантом",
		"Обращайся к кандидату только на `вы`", "вы указали", "вы ответили",
		"Существенного пробела по этому вопросу нет", "Не выдумывай недостаток",
		"КДИР является рекомендацией", "Целевой уровень: мидл",
	} {
		if !strings.Contains(gotUserContent, want) {
			t.Errorf("expected the request's user message to contain %q, got:\n%s", want, gotUserContent)
		}
	}
	if strings.Contains(gotUserContent, "junior-ref") || strings.Contains(gotUserContent, "senior-ref") {
		t.Errorf("expected only the middle reference answer, got:\n%s", gotUserContent)
	}
}

func TestBuildFollowupEvaluationContext_IncludesBothAnswersAndPinsPhase(t *testing.T) {
	question := &db.QuestionBank{
		ID: 8, QuestionText: "Как описать API?", Topic: "интеграции",
		AnswerJunior: "jun", AnswerMiddle: "mid", AnswerSenior: "sen",
	}
	got := BuildFollowupEvaluationContext(
		StudentProfile{Grade: "мидл"},
		[]db.WeakZone{{ZoneText: "интеграции", Status: db.WeakZoneStatusHypothesis}},
		"первый ответ", "почему выбрал REST?", "ответ на уточнение", question,
	)

	for _, want := range []string{
		"ФАЗА 3", "первый уточняющий вопрос", "первый ответ", "почему выбрал REST?",
		"ответ на уточнение", "только одну рамку", "Не спрашивай грейд",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("follow-up context does not contain %q:\n%s", want, got)
		}
	}
}

func TestBuildBlockEvaluationContext_IncludesThreeAnswersAndKDIR(t *testing.T) {
	question := &db.QuestionBank{ID: 8, QuestionText: "Как описать API?", Topic: "интеграции"}
	got := BuildBlockEvaluationContext(
		StudentProfile{TargetGrade: "мидл"}, nil,
		"основной ответ", "уточнение один", "ответ два",
		"уточнение два", "ответ три", question,
	)

	for _, want := range []string{
		"ФАЗА 3", "основной ответ", "уточнение один", "ответ два",
		"уточнение два", "ответ три", "БОЛЬШАЯ ОБРАТНАЯ СВЯЗЬ",
		"Итог", "Сильная сторона", "Главная точка роста", "Как усилить ответ",
		"КДИР", "рекомендацию по подаче", "WEAK_ZONE_STATUS",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("block context does not contain %q:\n%s", want, got)
		}
	}
}

func TestBuildEvaluationContext_UsesSeniorDepthWithoutTurningReferenceIntoChecklist(t *testing.T) {
	question := &db.QuestionBank{
		QuestionText: "Как выбрать способ интеграции?",
		AnswerSenior: "Один из возможных ответов",
	}
	got := BuildEvaluationContext(
		StudentProfile{TargetGrade: "сеньор"}, nil,
		"Выбрал очередь и объяснил причины", question,
	)

	for _, want := range []string{
		"Целевой уровень: сеньор",
		"компромиссы, риски и альтернативы только там",
		"не чек-листом терминов",
		"Не добавляй обязательные детали",
		"Короткий корректный ответ не называй неправильным",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("senior evaluation context does not contain %q:\n%s", want, got)
		}
	}
}

func TestEvaluationContextsExposeOnlyTheReferenceForTheCurrentQuestion(t *testing.T) {
	question := &db.QuestionBank{
		QuestionText: "Как выбрать архитектуру?",
		Followup1:    "Что изменится при росте нагрузки?",
		Followup2:    "Что изменится при сокращении бюджета?",
		AnswerMiddle: "Пример к основному вопросу: основной ориентир\n" +
			"Пример к первому дожиму: ориентир первого дожима\n" +
			"Пример ко второму дожиму: ориентир второго дожима",
	}
	profile := StudentProfile{TargetGrade: "мидл"}

	primary := BuildEvaluationContext(profile, nil, "ответ", question)
	if !strings.Contains(primary, "основной ориентир") || strings.Contains(primary, "ориентир первого дожима") ||
		strings.Contains(primary, "Что изменится при росте нагрузки?") {
		t.Fatalf("primary evaluation leaked a future follow-up or its reference:\n%s", primary)
	}

	followup := BuildFollowupEvaluationContext(profile, nil, "ответ", question.Followup1, "уточнение", question)
	if !strings.Contains(followup, "ориентир первого дожима") || strings.Contains(followup, "основной ориентир") ||
		strings.Contains(followup, "ориентир второго дожима") || strings.Contains(followup, question.Followup2) {
		t.Fatalf("first follow-up evaluation received references outside its scope:\n%s", followup)
	}

	block := BuildBlockEvaluationContext(profile, nil, "ответ", question.Followup1, "уточнение 1", question.Followup2, "уточнение 2", question)
	for _, want := range []string{"основной ориентир", "ориентир первого дожима", "ориентир второго дожима"} {
		if !strings.Contains(block, want) {
			t.Fatalf("block evaluation is missing %q:\n%s", want, block)
		}
	}
}

func TestEvaluationGuidelinesAreTopicIndependent(t *testing.T) {
	tests := []struct {
		name     string
		topic    string
		question string
	}{
		{name: "diagram", topic: "архитектура", question: "Как используешь ERD или C4?"},
		{name: "token", topic: "безопасность", question: "Откуда сервис получает роли для токена?"},
		{name: "requirements", topic: "требования", question: "Как проверяешь полноту требований?"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildEvaluationContext(
				StudentProfile{TargetGrade: "мидл"}, nil, "короткий ответ",
				&db.QuestionBank{Topic: tt.topic, QuestionText: tt.question, AnswerMiddle: "пример"},
			)
			for _, want := range []string{
				"живой интервьюер и наставник",
				"Существенного пробела по этому вопросу нет",
				"Не выдумывай недостаток",
				"Не добавляй обязательные детали",
				"КДИР является рекомендацией",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("topic %q did not receive common rule %q:\n%s", tt.topic, want, got)
				}
			}
		})
	}
}

func TestStrongRequirementsAnswerMayBeAcceptedWithoutInventedGap(t *testing.T) {
	answer := "Уточняю бизнес-цель и KPI, пользователей, интеграции, ограничения, границы проекта, сроки и бюджет."
	got := BuildEvaluationContext(
		StudentProfile{TargetGrade: "мидл"}, nil, answer,
		&db.QuestionBank{
			Topic:           "требования",
			QuestionText:    "Какие вопросы вы задаете бизнесу перед началом проекта?",
			AnswerMiddle:    "Цель, пользователь, критерии успеха, сроки и бюджет.",
			AnswerSenior:    "Цель, пользователь, критерии успеха, сроки и бюджет.",
			QuestionContext: "Интервьюер проверяет работу с требованиями.",
		},
	)

	for _, want := range []string{
		answer,
		"Не объявляй отсутствующим то, что он уже назвал напрямую или по смыслу",
		"Существенного пробела по этому вопросу нет",
		"сильный ответ разрешено принять без критики",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("strong-answer context does not contain %q:\n%s", want, got)
		}
	}
}

func TestBuildSummaryContext_UsesPersistedAttempts(t *testing.T) {
	got := BuildSummaryContext(
		QualificationProfile{
			Grade: "джун", Direction: "финтех", Experience: "мало Kafka",
			InterviewTarget: "собеседование завтра",
		},
		[]db.WeakZone{{ZoneText: "бд", Status: db.WeakZoneStatusConfirmed}},
		[]db.QuestionAttemptReport{{
			QuestionText: "Что такое индекс?", Topic: "бд",
			PrimaryAnswer: "первый", FollowupQuestion: "когда вредит?",
			FollowupAnswer: "второй", Followup2Question: "как измерить?",
			Followup2Answer: "третий", FinalFeedback: "нужно больше конкретики",
		}},
	)

	for _, want := range []string{
		"ФАЗА 4", "Что такое индекс?", "первый", "когда вредит?", "второй", "как измерить?", "третий",
		"нужно больше конкретики", "бд: confirmed", "финтех", "мало Kafka", "собеседование завтра",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary context does not contain %q:\n%s", want, got)
		}
	}
}
