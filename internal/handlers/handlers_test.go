package handlers

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	tele "gopkg.in/telebot.v3"

	"sa-hr-bot/internal/db"
	"sa-hr-bot/internal/llm"
)

// newCapturingLogger returns a logger whose output can be inspected,
// for tests that need to assert on what actually got logged rather than
// just that a handler didn't crash.
func newCapturingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// fakeContext implements just enough of tele.Context for the handlers
// under test. Embedding the (nil) interface satisfies the rest of the
// method set; any call to an unoverridden method would panic, which is
// fine since handlers.go never calls them.
type fakeContext struct {
	tele.Context
	sender *tele.User
	text   string
	args   []string

	sent         []string
	sentDocs     []*tele.Document
	sentDocBytes [][]byte
	responded    bool
}

func (f *fakeContext) Sender() *tele.User { return f.sender }
func (f *fakeContext) Text() string       { return f.text }
func (f *fakeContext) Args() []string     { return f.args }

func (f *fakeContext) Send(what interface{}, _ ...interface{}) error {
	switch v := what.(type) {
	case string:
		f.sent = append(f.sent, v)
	case *tele.Document:
		f.sentDocs = append(f.sentDocs, v)
		// Read the file now: handleReport removes it (defer os.Remove)
		// right after Send returns, so this is the only chance to
		// inspect what was actually written.
		if v.FileLocal != "" {
			if data, err := os.ReadFile(v.FileLocal); err == nil {
				f.sentDocBytes = append(f.sentDocBytes, data)
			}
		}
	}
	return nil
}

func (f *fakeContext) Respond(_ ...*tele.CallbackResponse) error {
	f.responded = true
	return nil
}

func newCtx(telegramID int64, text string, args ...string) *fakeContext {
	return &fakeContext{
		sender: &tele.User{ID: telegramID, FirstName: "Тест"},
		text:   text,
		args:   args,
	}
}

// fakeRepo is an in-memory stand-in for db.Repository covering exactly
// the behavior handlers.go relies on.
type fakeRepo struct {
	students  map[int64]*db.Student
	sessions  map[int64]*db.Session
	nextID    int64
	codes     map[string]bool // code -> used
	summaries []db.SessionSummary
	weakZones map[int64][]db.WeakZone
	questions map[int64]*db.QuestionBank

	// panicOnAnyCall makes every method below panic immediately, for
	// tests proving recoverMiddleware actually wraps a given endpoint:
	// every handler in this package touches the repository as its very
	// first action, so this is a uniform way to trigger a panic deep
	// inside whichever handler a test is exercising.
	panicOnAnyCall bool
}

func (r *fakeRepo) maybePanic() {
	if r.panicOnAnyCall {
		panic("intentional test panic from fakeRepo")
	}
}

func newFakeRepo(codes ...string) *fakeRepo {
	m := make(map[string]bool, len(codes))
	for _, c := range codes {
		m[c] = false
	}
	return &fakeRepo{
		students:  make(map[int64]*db.Student),
		sessions:  make(map[int64]*db.Session),
		codes:     m,
		weakZones: make(map[int64][]db.WeakZone),
		questions: make(map[int64]*db.QuestionBank),
	}
}

func (r *fakeRepo) sessionByID(id int64) (*db.Session, error) {
	s, ok := r.sessions[id]
	if !ok {
		return nil, errors.New("session not found")
	}
	cp := *s
	return &cp, nil
}

func (r *fakeRepo) CreateStudentIfAccessCodeValid(_ context.Context, telegramID int64, name, code string) (*db.Student, error) {
	r.maybePanic()
	used, ok := r.codes[code]
	if !ok || used {
		return nil, db.ErrAccessCodeInvalid
	}
	if _, exists := r.students[telegramID]; exists {
		return nil, db.ErrStudentAlreadyRegistered
	}
	r.codes[code] = true
	s := &db.Student{TelegramID: telegramID, Name: name, AccessCode: code, CreatedAt: time.Now()}
	r.students[telegramID] = s
	return s, nil
}

func (r *fakeRepo) GetStudentByTelegramID(_ context.Context, telegramID int64) (*db.Student, error) {
	r.maybePanic()
	s, ok := r.students[telegramID]
	if !ok {
		return nil, db.ErrStudentNotFound
	}
	return s, nil
}

func (r *fakeRepo) GetActiveSession(_ context.Context, studentID int64) (*db.Session, error) {
	r.maybePanic()
	for _, s := range r.sessions {
		if s.StudentID == studentID && s.EndedAt == nil {
			cp := *s
			return &cp, nil
		}
	}
	return nil, db.ErrNoActiveSession
}

func (r *fakeRepo) StartSession(_ context.Context, studentID int64) (*db.Session, error) {
	r.maybePanic()
	r.nextID++
	s := &db.Session{ID: r.nextID, StudentID: studentID, Status: db.SessionStatusInstruction, StartedAt: time.Now()}
	r.sessions[s.ID] = s
	cp := *s
	return &cp, nil
}

func (r *fakeRepo) EndSession(_ context.Context, sessionID int64, status string) error {
	r.maybePanic()
	s, ok := r.sessions[sessionID]
	if !ok {
		return errors.New("session not found")
	}
	now := time.Now()
	s.EndedAt = &now
	s.Status = status
	return nil
}

func (r *fakeRepo) AdvancePhase(_ context.Context, sessionID int64, newStatus string) error {
	r.maybePanic()
	s, ok := r.sessions[sessionID]
	if !ok {
		return errors.New("session not found")
	}
	s.Status = newStatus
	s.CycleCount = 0
	s.CurrentQuestionID = nil
	return nil
}

func (r *fakeRepo) SetCurrentQuestionID(_ context.Context, sessionID int64, questionID *int64) error {
	r.maybePanic()
	s, ok := r.sessions[sessionID]
	if !ok {
		return errors.New("session not found")
	}
	s.CurrentQuestionID = questionID
	return nil
}

func (r *fakeRepo) GetQuestionByID(_ context.Context, id int64) (*db.QuestionBank, error) {
	r.maybePanic()
	q, ok := r.questions[id]
	if !ok {
		return nil, db.ErrQuestionNotFound
	}
	return q, nil
}

func (r *fakeRepo) IncrementCycleCount(_ context.Context, sessionID int64) (int, error) {
	r.maybePanic()
	s, ok := r.sessions[sessionID]
	if !ok {
		return 0, errors.New("session not found")
	}
	s.CycleCount++
	return s.CycleCount, nil
}

func (r *fakeRepo) SetQualificationFields(_ context.Context, sessionID int64, currentGrade, targetGrade, studentRequest, selfAssessment string) error {
	r.maybePanic()
	s, ok := r.sessions[sessionID]
	if !ok {
		return errors.New("session not found")
	}
	if currentGrade != "" {
		s.CurrentGrade = currentGrade
	}
	if targetGrade != "" {
		s.Grade = targetGrade
	}
	if studentRequest != "" {
		s.StudentRequest = studentRequest
	}
	if selfAssessment != "" {
		s.SelfAssessment = selfAssessment
	}
	return nil
}

func (r *fakeRepo) SetWeakTopics(_ context.Context, sessionID int64, topics string) error {
	r.maybePanic()
	s, ok := r.sessions[sessionID]
	if !ok {
		return errors.New("session not found")
	}
	s.WeakTopics = topics
	return nil
}

func (r *fakeRepo) GetWeakZones(_ context.Context, studentID int64) ([]db.WeakZone, error) {
	r.maybePanic()
	return r.weakZones[studentID], nil
}

func (r *fakeRepo) SaveSummary(_ context.Context, sessionID int64, summaryText string) (*db.SessionSummary, error) {
	r.maybePanic()
	sum := db.SessionSummary{ID: int64(len(r.summaries)) + 1, SessionID: sessionID, SummaryText: summaryText}
	r.summaries = append(r.summaries, sum)
	return &sum, nil
}

// GetStudentReports derives the same aggregate the real repository
// computes in SQL (completed session count, last session date, current
// weak-zone map), from the fake's in-memory state.
func (r *fakeRepo) GetStudentReports(_ context.Context) ([]db.StudentReport, error) {
	r.maybePanic()
	reports := make([]db.StudentReport, 0, len(r.students))
	for id, s := range r.students {
		rep := db.StudentReport{TelegramID: id, Name: s.Name, WeakZones: r.weakZones[id]}
		var last *time.Time
		for _, sess := range r.sessions {
			if sess.StudentID != id {
				continue
			}
			if sess.Status == db.SessionStatusCompleted {
				rep.CompletedSessions++
			}
			st := sess.StartedAt
			if last == nil || st.After(*last) {
				last = &st
			}
		}
		rep.LastSessionAt = last
		reports = append(reports, rep)
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].TelegramID < reports[j].TelegramID })
	return reports, nil
}

// fakeLLM is an in-memory stand-in for *llm.Service.
type fakeLLM struct {
	replies   []string
	question  *db.QuestionBank
	pickCalls int
	evalCalls int
	pickErr   error // if set, returned by the next PickQuestion call, then cleared

	lastEvalQuestion             *db.QuestionBank
	lastPickGrade, lastPickTopic string
}

func (f *fakeLLM) nextReply() *llm.Reply {
	if len(f.replies) == 0 {
		return &llm.Reply{Text: "ok"}
	}
	text := f.replies[0]
	f.replies = f.replies[1:]
	return &llm.Reply{Text: text}
}

func (f *fakeLLM) Reply(_ context.Context, _ string) (*llm.Reply, error) {
	return f.nextReply(), nil
}

func (f *fakeLLM) Evaluate(_ context.Context, _ llm.StudentProfile, _ []db.WeakZone, _ string, question *db.QuestionBank) (*llm.Reply, error) {
	f.evalCalls++
	f.lastEvalQuestion = question
	return f.nextReply(), nil
}

func (f *fakeLLM) PickQuestion(_ context.Context, grade, topic string) (*db.QuestionBank, error) {
	f.pickCalls++
	f.lastPickGrade, f.lastPickTopic = grade, topic
	if f.pickErr != nil {
		err := f.pickErr
		f.pickErr = nil
		return nil, err
	}
	return f.question, nil
}

func TestFullSessionFlow(t *testing.T) {
	repo := newFakeRepo("SA2026-TEST")
	lm := &fakeLLM{question: &db.QuestionBank{ID: 1, QuestionText: "q", Grade: "мидл", Topic: "бд"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(repo, lm, logger, 2, nil) // small cycle limit keeps the test short

	const telegramID = 42
	ctx := context.Background()

	// Invalid code: no student created, no session started.
	badCtx := newCtx(telegramID, "/start WRONG", "WRONG")
	if err := h.handleStart(badCtx); err != nil {
		t.Fatalf("handleStart (bad code): %v", err)
	}
	if len(badCtx.sent) != 1 || !strings.Contains(badCtx.sent[0], "Код не подходит") {
		t.Fatalf("expected invalid code message, got %v", badCtx.sent)
	}
	if _, err := repo.GetStudentByTelegramID(ctx, telegramID); !errors.Is(err, db.ErrStudentNotFound) {
		t.Fatalf("student should not exist after an invalid code")
	}

	// Valid code: registers, starts a session, sends the phase 0
	// instruction, and lands in QUALIFICATION.
	lm.replies = []string{"Инструкция фазы 0"}
	startCtx := newCtx(telegramID, "/start SA2026-TEST", "SA2026-TEST")
	if err := h.handleStart(startCtx); err != nil {
		t.Fatalf("handleStart: %v", err)
	}
	if len(startCtx.sent) != 1 || startCtx.sent[0] != "Инструкция фазы 0" {
		t.Fatalf("unexpected instruction message: %v", startCtx.sent)
	}
	session, err := repo.GetActiveSession(ctx, telegramID)
	if err != nil {
		t.Fatalf("GetActiveSession: %v", err)
	}
	if session.Status != db.SessionStatusQualification {
		t.Fatalf("expected QUALIFICATION after instruction, got %s", session.Status)
	}

	// Qualification: transitions as soon as all four fields are known,
	// not on a fixed turn count. The model reveals one field per turn
	// here; after 3 turns, 3 of 4 are known and the FSM must stay put.
	lm.replies = []string{
		"Какой у тебя грейд сейчас?\nCURRENT_GRADE: джун",
		"На какой грейд претендуешь?\nTARGET_GRADE: мидл",
		"Что хочешь получить от тренировки?\nREQUEST: подготовиться к собеседованию в Т-Банк",
	}
	for i := 0; i < 3; i++ {
		if err := h.handleMessage(newCtx(telegramID, "ответ на квалификацию")); err != nil {
			t.Fatalf("handleMessage qualification turn %d: %v", i, err)
		}
	}
	session, _ = repo.GetActiveSession(ctx, telegramID)
	if session.Status != db.SessionStatusQualification || session.CycleCount != 3 {
		t.Fatalf("expected QUALIFICATION cycle_count=3, got status=%s count=%d", session.Status, session.CycleCount)
	}
	if session.CurrentGrade != "джун" || session.Grade != "мидл" || session.StudentRequest == "" {
		t.Fatalf("expected 3 of 4 fields captured, got %+v", session)
	}

	// 4th turn: the model reveals the last missing field
	// (SELF_ASSESSMENT), so the FSM must move to AUDIT right away, well
	// under the emergency cap of 6.
	lm.replies = []string{"Понял тебя.\nSELF_ASSESSMENT: хорошо знает БД, слабо в интеграциях"}
	qualDoneCtx := newCtx(telegramID, "последний ответ квалификации")
	if err := h.handleMessage(qualDoneCtx); err != nil {
		t.Fatalf("handleMessage qualification turn 4: %v", err)
	}
	if len(qualDoneCtx.sent) != 1 ||
		strings.Contains(qualDoneCtx.sent[0], "SELF_ASSESSMENT:") ||
		strings.Contains(qualDoneCtx.sent[0], "CURRENT_GRADE:") {
		t.Fatalf("expected markers stripped from the message shown to the student, got %q", qualDoneCtx.sent)
	}
	session, _ = repo.GetActiveSession(ctx, telegramID)
	if session.Status != db.SessionStatusAudit {
		t.Fatalf("expected AUDIT as soon as all 4 fields are known (turn 4, cap is 6), got %s", session.Status)
	}
	if session.CurrentGrade != "джун" || session.Grade != "мидл" ||
		session.StudentRequest == "" || session.SelfAssessment == "" {
		t.Fatalf("expected all 4 qualification fields captured, got %+v", session)
	}

	// Seed the question bank fake so GetQuestionByID can resolve the
	// question PickQuestion hands out, with real reference answers to
	// verify Evaluate receives the actual asked question, not a fresh
	// random pick.
	repo.questions[1] = &db.QuestionBank{
		ID: 1, QuestionText: "q", Grade: "мидл", Topic: "бд",
		AnswerJunior: "junior answer", AnswerMiddle: "middle answer", AnswerSenior: "senior answer",
	}

	// Audit: cap is 1, a single message moves straight to QUESTION_CYCLE.
	// The reply also flags weak topics, which must end up as a priority
	// filter for PickQuestion once the question cycle starts.
	lm.replies = []string{"▸ МИНИ-АУДИТ\n...\nWEAK_TOPICS: бд,интеграции"}
	auditCtx := newCtx(telegramID, "ок")
	if err := h.handleMessage(auditCtx); err != nil {
		t.Fatalf("handleMessage audit: %v", err)
	}
	if len(auditCtx.sent) != 1 || strings.Contains(auditCtx.sent[0], "WEAK_TOPICS") {
		t.Fatalf("expected WEAK_TOPICS marker stripped from the audit message, got %q", auditCtx.sent)
	}
	session, _ = repo.GetActiveSession(ctx, telegramID)
	if session.Status != db.SessionStatusQuestionCycle {
		t.Fatalf("expected QUESTION_CYCLE after audit, got %s", session.Status)
	}
	if session.CurrentQuestionID != nil {
		t.Fatalf("expected no current question right after entering QUESTION_CYCLE, got %v", session.CurrentQuestionID)
	}
	if session.WeakTopics != "бд,интеграции" {
		t.Fatalf("expected weak_topics captured from the audit reply, got %q", session.WeakTopics)
	}

	// First message in the cycle: no question asked yet, so the bot
	// picks one and sends it as-is, without calling the model at all.
	askCtx := newCtx(telegramID, "готов")
	if err := h.handleMessage(askCtx); err != nil {
		t.Fatalf("handleMessage ask question 1: %v", err)
	}
	if len(askCtx.sent) != 1 || askCtx.sent[0] != "q" {
		t.Fatalf("expected the raw bank question to be sent, got %v", askCtx.sent)
	}
	session, _ = repo.GetActiveSession(ctx, telegramID)
	if session.CurrentQuestionID == nil || *session.CurrentQuestionID != 1 {
		t.Fatalf("expected current_question_id=1, got %v", session.CurrentQuestionID)
	}
	if session.CycleCount != 0 {
		t.Fatalf("expected cycle_count still 0 before any answer is graded, got %d", session.CycleCount)
	}
	if lm.lastPickTopic != "бд" {
		t.Fatalf("expected PickQuestion to be called with the first weak topic (\"бд\") as a priority filter, got %q", lm.lastPickTopic)
	}
	if lm.pickCalls != 1 {
		t.Fatalf("expected 1 PickQuestion call so far, got %d", lm.pickCalls)
	}

	// Second message: the answer to question 1. Grading it reaches
	// cycle_count=1 (< limit 2), so the bot chains straight into asking
	// the next question in the same update.
	lm.replies = []string{"▸ ТВОЙ УРОВЕНЬ ПО ЭТОМУ ВОПРОСУ: мидл\n..."}
	answerCtx := newCtx(telegramID, "мой ответ на вопрос 1")
	if err := h.handleMessage(answerCtx); err != nil {
		t.Fatalf("handleMessage answer 1: %v", err)
	}
	if len(answerCtx.sent) != 2 {
		t.Fatalf("expected 2 messages (evaluation + next question), got %v", answerCtx.sent)
	}
	if !strings.Contains(answerCtx.sent[0], "ТВОЙ УРОВЕНЬ") {
		t.Fatalf("expected the evaluation frame first, got %q", answerCtx.sent[0])
	}
	if answerCtx.sent[1] != "q" {
		t.Fatalf("expected the next bank question second, got %q", answerCtx.sent[1])
	}
	if lm.lastEvalQuestion == nil || lm.lastEvalQuestion.ID != 1 || lm.lastEvalQuestion.AnswerSenior != "senior answer" {
		t.Fatalf("expected Evaluate to receive the question fetched by id with its reference answers, got %+v", lm.lastEvalQuestion)
	}
	session, _ = repo.GetActiveSession(ctx, telegramID)
	if session.Status != db.SessionStatusQuestionCycle || session.CycleCount != 1 {
		t.Fatalf("expected QUESTION_CYCLE cycle_count=1, got status=%s count=%d", session.Status, session.CycleCount)
	}
	if session.CurrentQuestionID == nil {
		t.Fatalf("expected a new current_question_id after chaining into the next question")
	}
	if lm.evalCalls != 1 || lm.pickCalls != 2 {
		t.Fatalf("expected 1 Evaluate and 2 PickQuestion calls so far, got eval=%d pick=%d", lm.evalCalls, lm.pickCalls)
	}

	// Third message: the answer to question 2. This is the 2nd graded
	// cycle, hitting the limit, so it chains straight into SUMMARY.
	lm.replies = []string{"▸ ТВОЙ УРОВЕНЬ ПО ЭТОМУ ВОПРОСУ: джун\n...", "Итоговый отчет"}
	finalCtx := newCtx(telegramID, "мой ответ на вопрос 2")
	if err := h.handleMessage(finalCtx); err != nil {
		t.Fatalf("handleMessage answer 2: %v", err)
	}
	if len(finalCtx.sent) != 2 {
		t.Fatalf("expected 2 messages sent (evaluation + summary), got %v", finalCtx.sent)
	}
	if finalCtx.sent[1] != "Итоговый отчет" {
		t.Fatalf("expected the summary as the second message, got %q", finalCtx.sent[1])
	}

	if _, err := repo.GetActiveSession(ctx, telegramID); !errors.Is(err, db.ErrNoActiveSession) {
		t.Fatalf("expected no active session after summary, got %v", err)
	}
	if len(repo.summaries) != 1 || repo.summaries[0].SummaryText != "Итоговый отчет" {
		t.Fatalf("expected the summary to be saved, got %v", repo.summaries)
	}
	if lm.evalCalls != 2 {
		t.Fatalf("expected 2 Evaluate calls total, got %d", lm.evalCalls)
	}
	if lm.pickCalls != 2 {
		t.Fatalf("expected exactly 2 PickQuestion calls total (one per question actually asked), got %d", lm.pickCalls)
	}
}

func TestRestartDuringActiveSession(t *testing.T) {
	repo := newFakeRepo("SA2026-TEST")
	lm := &fakeLLM{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(repo, lm, logger, 8, nil)

	const telegramID = 7
	ctx := context.Background()

	lm.replies = []string{"инструкция"}
	if err := h.handleStart(newCtx(telegramID, "/start SA2026-TEST", "SA2026-TEST")); err != nil {
		t.Fatalf("initial start: %v", err)
	}

	// /start again while a session is active: offer the choice, touch
	// nothing yet.
	again := newCtx(telegramID, "/start")
	if err := h.handleStart(again); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if len(again.sent) != 1 || !strings.Contains(again.sent[0], "Начать заново или продолжить") {
		t.Fatalf("expected restart-or-continue prompt, got %v", again.sent)
	}

	firstSession, err := repo.GetActiveSession(ctx, telegramID)
	if err != nil {
		t.Fatalf("GetActiveSession: %v", err)
	}

	// Tap "Начать заново".
	lm.replies = []string{"новая инструкция"}
	restartCtx := newCtx(telegramID, "")
	if err := h.handleRestartCallback(restartCtx); err != nil {
		t.Fatalf("handleRestartCallback: %v", err)
	}
	if !restartCtx.responded {
		t.Fatalf("expected the callback query to be acknowledged via Respond")
	}

	oldSession, err := repo.sessionByID(firstSession.ID)
	if err != nil {
		t.Fatalf("lookup old session: %v", err)
	}
	if oldSession.Status != db.SessionStatusAbandoned || oldSession.EndedAt == nil {
		t.Fatalf("expected the old session to be abandoned, got status=%s ended=%v", oldSession.Status, oldSession.EndedAt)
	}

	newSession, err := repo.GetActiveSession(ctx, telegramID)
	if err != nil {
		t.Fatalf("expected a new active session: %v", err)
	}
	if newSession.ID == firstSession.ID {
		t.Fatalf("expected a fresh session, got the same ID as the abandoned one")
	}
}

func TestContinueDuringActiveSession(t *testing.T) {
	repo := newFakeRepo("SA2026-TEST")
	lm := &fakeLLM{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(repo, lm, logger, 8, nil)

	const telegramID = 9
	ctx := context.Background()

	lm.replies = []string{"инструкция"}
	if err := h.handleStart(newCtx(telegramID, "/start SA2026-TEST", "SA2026-TEST")); err != nil {
		t.Fatalf("initial start: %v", err)
	}
	before, err := repo.GetActiveSession(ctx, telegramID)
	if err != nil {
		t.Fatalf("GetActiveSession: %v", err)
	}

	continueCtx := newCtx(telegramID, "")
	if err := h.handleContinueCallback(continueCtx); err != nil {
		t.Fatalf("handleContinueCallback: %v", err)
	}
	if !continueCtx.responded {
		t.Fatalf("expected the callback query to be acknowledged via Respond")
	}

	after, err := repo.GetActiveSession(ctx, telegramID)
	if err != nil {
		t.Fatalf("GetActiveSession after continue: %v", err)
	}
	if after.ID != before.ID {
		t.Fatalf("continue should not replace the session: before=%d after=%d", before.ID, after.ID)
	}
}

func TestContinueCallback_NoActiveSession(t *testing.T) {
	repo := newFakeRepo("SA2026-TEST")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(repo, &fakeLLM{}, logger, 8, nil)

	const telegramID = 10
	if _, err := repo.CreateStudentIfAccessCodeValid(context.Background(), telegramID, "Тест", "SA2026-TEST"); err != nil {
		t.Fatalf("seed student: %v", err)
	}
	// Deliberately no session started: this is the race handleContinueCallback's
	// active-session check exists for (see its doc comment).

	continueCtx := newCtx(telegramID, "")
	if err := h.handleContinueCallback(continueCtx); err != nil {
		t.Fatalf("handleContinueCallback: %v", err)
	}
	if len(continueCtx.sent) != 1 || !strings.Contains(continueCtx.sent[0], "Активной сессии уже нет") {
		t.Fatalf("expected a no-active-session message instead of the generic continue message, got %v", continueCtx.sent)
	}
}

// TestRestartCallback_ResetsAllSessionFields is the regression test for
// point 2 of the reliability audit: a restart must give a genuinely
// clean session, not one that inherits leftover data from a previous,
// possibly stuck, attempt. It seeds the old session with every field a
// partial/stuck attempt could have populated (mirroring the real
// telegram_id 755067732 case from the earlier incident) and asserts the
// new session is blank across all of them.
func TestRestartCallback_ResetsAllSessionFields(t *testing.T) {
	repo := newFakeRepo("SA2026-TEST")
	lm := &fakeLLM{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(repo, lm, logger, 8, nil)

	const telegramID = 321
	ctx := context.Background()

	lm.replies = []string{"инструкция"}
	if err := h.handleStart(newCtx(telegramID, "/start SA2026-TEST", "SA2026-TEST")); err != nil {
		t.Fatalf("initial start: %v", err)
	}

	old, err := repo.GetActiveSession(ctx, telegramID)
	if err != nil {
		t.Fatalf("GetActiveSession: %v", err)
	}

	// Dirty every field a stuck or partially-completed attempt could
	// have left behind.
	questionID := int64(999)
	dirty := repo.sessions[old.ID]
	dirty.Status = db.SessionStatusQuestionCycle
	dirty.CycleCount = 5
	dirty.CurrentGrade = "джун"
	dirty.Grade = "мидл"
	dirty.StudentRequest = "старый запрос"
	dirty.SelfAssessment = "старая самооценка"
	dirty.WeakTopics = "бд,интеграции"
	dirty.CurrentQuestionID = &questionID

	lm.replies = []string{"новая инструкция"}
	restartCtx := newCtx(telegramID, "")
	if err := h.handleRestartCallback(restartCtx); err != nil {
		t.Fatalf("handleRestartCallback: %v", err)
	}

	fresh, err := repo.GetActiveSession(ctx, telegramID)
	if err != nil {
		t.Fatalf("GetActiveSession after restart: %v", err)
	}
	if fresh.ID == old.ID {
		t.Fatalf("expected a brand new session row, got the same ID as the abandoned one")
	}
	if fresh.Status != db.SessionStatusQualification {
		t.Errorf("status: got %s, want QUALIFICATION (set by startFreshSession after the phase-0 message)", fresh.Status)
	}
	if fresh.CycleCount != 0 {
		t.Errorf("cycle_count not reset: got %d", fresh.CycleCount)
	}
	if fresh.CurrentGrade != "" {
		t.Errorf("current_grade not reset: got %q", fresh.CurrentGrade)
	}
	if fresh.Grade != "" {
		t.Errorf("grade (target grade) not reset: got %q", fresh.Grade)
	}
	if fresh.StudentRequest != "" {
		t.Errorf("student_request not reset: got %q", fresh.StudentRequest)
	}
	if fresh.SelfAssessment != "" {
		t.Errorf("self_assessment not reset: got %q", fresh.SelfAssessment)
	}
	if fresh.WeakTopics != "" {
		t.Errorf("weak_topics not reset: got %q", fresh.WeakTopics)
	}
	if fresh.CurrentQuestionID != nil {
		t.Errorf("current_question_id not reset: got %v", *fresh.CurrentQuestionID)
	}

	oldAfter, err := repo.sessionByID(old.ID)
	if err != nil {
		t.Fatalf("lookup old session: %v", err)
	}
	if oldAfter.Status != db.SessionStatusAbandoned {
		t.Errorf("expected the old session to end up ABANDONED, got %s", oldAfter.Status)
	}
	// The old (dirty) row itself is untouched by the reset, only ended:
	// AdvancePhase/EndSession never rewrite these on the abandoned row,
	// which is correct - they document what that attempt actually did.
	if oldAfter.CurrentGrade != "джун" {
		t.Errorf("did not expect the abandoned session's own data to be touched, got current_grade=%q", oldAfter.CurrentGrade)
	}
}

func TestExtractQualificationMarkers(t *testing.T) {
	text := "На какой грейд претендуешь?\nCURRENT_GRADE: джун\nTARGET_GRADE: мидл\n" +
		"REQUEST: подготовиться к переходу\nSELF_ASSESSMENT: слаб в архитектуре"

	currentGrade, targetGrade, request, selfAssessment, cleaned := extractQualificationMarkers(text)

	if currentGrade != "джун" {
		t.Errorf("currentGrade = %q, want %q", currentGrade, "джун")
	}
	if targetGrade != "мидл" {
		t.Errorf("targetGrade = %q, want %q", targetGrade, "мидл")
	}
	if request != "подготовиться к переходу" {
		t.Errorf("request = %q, want %q", request, "подготовиться к переходу")
	}
	if selfAssessment != "слаб в архитектуре" {
		t.Errorf("selfAssessment = %q, want %q", selfAssessment, "слаб в архитектуре")
	}
	for _, marker := range []string{"CURRENT_GRADE", "TARGET_GRADE", "REQUEST", "SELF_ASSESSMENT"} {
		if strings.Contains(cleaned, marker) {
			t.Errorf("cleaned still contains marker %q: %q", marker, cleaned)
		}
	}
	if !strings.Contains(cleaned, "На какой грейд претендуешь?") {
		t.Errorf("cleaned lost the actual message: %q", cleaned)
	}
}

func TestExtractQualificationMarkers_Partial(t *testing.T) {
	text := "Понял, а что хочешь получить от тренировки?\nCURRENT_GRADE: мидл"

	currentGrade, targetGrade, request, selfAssessment, cleaned := extractQualificationMarkers(text)

	if currentGrade != "мидл" {
		t.Errorf("currentGrade = %q, want %q", currentGrade, "мидл")
	}
	if targetGrade != "" || request != "" || selfAssessment != "" {
		t.Errorf("expected only currentGrade set, got targetGrade=%q request=%q selfAssessment=%q",
			targetGrade, request, selfAssessment)
	}
	if strings.Contains(cleaned, "CURRENT_GRADE") {
		t.Errorf("cleaned still contains the marker: %q", cleaned)
	}
}

func TestExtractQualificationMarkers_NoMarkers(t *testing.T) {
	text := "Обычный ответ без меток."

	currentGrade, targetGrade, request, selfAssessment, cleaned := extractQualificationMarkers(text)

	if currentGrade != "" || targetGrade != "" || request != "" || selfAssessment != "" {
		t.Errorf("expected all fields empty, got %q/%q/%q/%q", currentGrade, targetGrade, request, selfAssessment)
	}
	if cleaned != text {
		t.Errorf("cleaned = %q, want unchanged %q", cleaned, text)
	}
}

func TestExtractWeakTopics(t *testing.T) {
	text := "▸ МИНИ-АУДИТ\nвероятно, слабое место: архитектура\nWEAK_TOPICS: бд,архитектура"

	topics, cleaned := extractWeakTopics(text)

	if strings.Join(topics, ",") != "бд,архитектура" {
		t.Errorf("topics = %v, want [бд архитектура]", topics)
	}
	if strings.Contains(cleaned, "WEAK_TOPICS") {
		t.Errorf("cleaned still contains the marker: %q", cleaned)
	}
	if !strings.Contains(cleaned, "МИНИ-АУДИТ") {
		t.Errorf("cleaned lost the actual message: %q", cleaned)
	}
}

func TestExtractWeakTopics_DropsInvalidValues(t *testing.T) {
	text := "▸ МИНИ-АУДИТ\nWEAK_TOPICS: бд,кулинария,требования"

	topics, _ := extractWeakTopics(text)

	if strings.Join(topics, ",") != "бд,требования" {
		t.Errorf("expected the hallucinated topic to be dropped, got %v", topics)
	}
}

func TestExtractWeakTopics_NoMarker(t *testing.T) {
	text := "▸ МИНИ-АУДИТ\nобычный текст без меток"

	topics, cleaned := extractWeakTopics(text)

	if topics != nil {
		t.Errorf("expected no topics, got %v", topics)
	}
	if cleaned != text {
		t.Errorf("cleaned = %q, want unchanged %q", cleaned, text)
	}
}

// seedStudentWithReport plants a student, and optionally a completed
// session and weak zones, directly into the fake so /report tests don't
// have to run a full registration + FSM flow per student.
func seedStudentWithReport(repo *fakeRepo, telegramID int64, name string, completedSessions int, lastSessionAt time.Time, zones ...db.WeakZone) {
	repo.students[telegramID] = &db.Student{TelegramID: telegramID, Name: name, CreatedAt: lastSessionAt}
	for i := 0; i < completedSessions; i++ {
		repo.nextID++
		repo.sessions[repo.nextID] = &db.Session{
			ID: repo.nextID, StudentID: telegramID, Status: db.SessionStatusCompleted,
			StartedAt: lastSessionAt, EndedAt: &lastSessionAt,
		}
	}
	if len(zones) > 0 {
		repo.weakZones[telegramID] = zones
	}
}

func TestReport_NonAdminSilentlyIgnored(t *testing.T) {
	repo := newFakeRepo()
	seedStudentWithReport(repo, 1, "Иван", 1, time.Now())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(repo, &fakeLLM{}, logger, 8, []int64{999}) // 999 is admin, not our caller

	const nonAdminID = 42
	ctx := newCtx(nonAdminID, "/report")
	if err := h.handleReport(ctx); err != nil {
		t.Fatalf("handleReport: %v", err)
	}
	if len(ctx.sent) != 0 || len(ctx.sentDocs) != 0 {
		t.Fatalf("expected a non-admin caller to get no reply at all, got sent=%v docs=%v", ctx.sent, ctx.sentDocs)
	}
}

func TestReport_AdminSmallListSendsText(t *testing.T) {
	repo := newFakeRepo()
	last := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	seedStudentWithReport(repo, 1, "Иван Иванов", 2, last,
		db.WeakZone{ZoneText: "интеграции", Status: db.WeakZoneStatusConfirmed},
		db.WeakZone{ZoneText: "требования", Status: db.WeakZoneStatusHypothesis},
	)
	seedStudentWithReport(repo, 2, "Мария Петрова", 0, time.Time{})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const adminID = 999
	h := New(repo, &fakeLLM{}, logger, 8, []int64{adminID})

	ctx := newCtx(adminID, "/report")
	if err := h.handleReport(ctx); err != nil {
		t.Fatalf("handleReport: %v", err)
	}
	if len(ctx.sentDocs) != 0 {
		t.Fatalf("expected a text reply, not a document, for 2 students, got %v", ctx.sentDocs)
	}
	if len(ctx.sent) != 1 {
		t.Fatalf("expected exactly one text message, got %v", ctx.sent)
	}

	text := ctx.sent[0]
	for _, want := range []string{
		"telegram_id: 1", "Иван Иванов", "Завершенных сессий: 2",
		"интеграции (confirmed)", "требования (hypothesis)",
		"telegram_id: 2", "Мария Петрова", "Завершенных сессий: 0",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("expected report text to contain %q, got:\n%s", want, text)
		}
	}
}

func TestReport_AdminLargeListSendsCSV(t *testing.T) {
	repo := newFakeRepo()
	const studentCount = 20
	for i := int64(1); i <= studentCount; i++ {
		seedStudentWithReport(repo, i, "Студент", 1, time.Now(),
			db.WeakZone{ZoneText: "бд", Status: db.WeakZoneStatusHypothesis})
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	const adminID = 999
	h := New(repo, &fakeLLM{}, logger, 8, []int64{adminID})

	ctx := newCtx(adminID, "/report")
	if err := h.handleReport(ctx); err != nil {
		t.Fatalf("handleReport: %v", err)
	}
	if len(ctx.sent) != 0 {
		t.Fatalf("expected a CSV document, not a text reply, for %d students, got %v", studentCount, ctx.sent)
	}
	if len(ctx.sentDocs) != 1 {
		t.Fatalf("expected exactly one document sent, got %d", len(ctx.sentDocs))
	}
	if !strings.HasSuffix(ctx.sentDocs[0].FileName, ".csv") {
		t.Fatalf("expected a .csv file name, got %q", ctx.sentDocs[0].FileName)
	}
	if len(ctx.sentDocBytes) != 1 {
		t.Fatalf("expected to have captured the CSV file's bytes")
	}

	records, err := csv.NewReader(strings.NewReader(string(ctx.sentDocBytes[0]))).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(records) != studentCount+1 {
		t.Fatalf("expected %d rows (header + %d students), got %d", studentCount+1, studentCount, len(records))
	}
	wantHeader := []string{"telegram_id", "name", "completed_sessions", "last_session_at", "weak_zones"}
	if strings.Join(records[0], ",") != strings.Join(wantHeader, ",") {
		t.Fatalf("unexpected csv header: %v", records[0])
	}
	if !strings.Contains(records[1][4], "бд (hypothesis)") {
		t.Fatalf("expected weak zones column to be populated, got %q", records[1][4])
	}
}

func TestQualification_EmergencyCapTransitionsWithPartialFields(t *testing.T) {
	repo := newFakeRepo("SA2026-TEST")
	lm := &fakeLLM{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(repo, lm, logger, 8, nil)

	const telegramID = 55
	ctx := context.Background()

	lm.replies = []string{"инструкция"}
	if err := h.handleStart(newCtx(telegramID, "/start SA2026-TEST", "SA2026-TEST")); err != nil {
		t.Fatalf("initial start: %v", err)
	}

	// The model never manages to reveal SELF_ASSESSMENT across 6 turns.
	lm.replies = []string{
		"вопрос 1\nCURRENT_GRADE: джун",
		"вопрос 2\nTARGET_GRADE: мидл",
		"вопрос 3\nREQUEST: подготовиться",
		"вопрос 4",
		"вопрос 5",
		"вопрос 6",
	}
	for i := 0; i < 6; i++ {
		if err := h.handleMessage(newCtx(telegramID, "ответ")); err != nil {
			t.Fatalf("handleMessage qualification turn %d: %v", i, err)
		}
	}

	session, err := repo.GetActiveSession(ctx, telegramID)
	if err != nil {
		t.Fatalf("GetActiveSession: %v", err)
	}
	if session.Status != db.SessionStatusAudit {
		t.Fatalf("expected the emergency cap (6) to force a transition to AUDIT, got %s", session.Status)
	}
	if session.SelfAssessment != "" {
		t.Fatalf("expected self_assessment to still be empty, got %q", session.SelfAssessment)
	}
	if session.CurrentGrade == "" || session.Grade == "" || session.StudentRequest == "" {
		t.Fatalf("expected the 3 fields that were revealed to be captured, got %+v", session)
	}
}

func TestPickPriorityTopic(t *testing.T) {
	repo := newFakeRepo()
	h := New(repo, &fakeLLM{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 8, nil)
	ctx := context.Background()

	const telegramID = 77
	student := &db.Student{TelegramID: telegramID}

	t.Run("weak zone tied to a concrete topic wins", func(t *testing.T) {
		repo.weakZones[telegramID] = []db.WeakZone{
			{ZoneText: "какой-то текст гипотезы", Status: db.WeakZoneStatusHypothesis},
			{ZoneText: "Архитектура", Status: db.WeakZoneStatusHypothesis}, // matches validTopics case-insensitively
		}
		session := &db.Session{WeakTopics: "бд,интеграции"}

		topic, err := h.pickPriorityTopic(ctx, student, session)
		if err != nil {
			t.Fatalf("pickPriorityTopic: %v", err)
		}
		if topic != "архитектура" {
			t.Fatalf("expected the weak-zone-tied topic to win, got %q", topic)
		}
	})

	t.Run("falls back to weak_topics when no weak zone matches a topic", func(t *testing.T) {
		repo.weakZones[telegramID] = []db.WeakZone{
			{ZoneText: "плохо формулирует нефункциональные требования в общении", Status: db.WeakZoneStatusHypothesis},
		}
		session := &db.Session{WeakTopics: "бд,интеграции"}

		topic, err := h.pickPriorityTopic(ctx, student, session)
		if err != nil {
			t.Fatalf("pickPriorityTopic: %v", err)
		}
		if topic != "бд" {
			t.Fatalf("expected the first weak_topics entry, got %q", topic)
		}
	})

	t.Run("empty when neither is available", func(t *testing.T) {
		repo.weakZones[telegramID] = nil
		session := &db.Session{}

		topic, err := h.pickPriorityTopic(ctx, student, session)
		if err != nil {
			t.Fatalf("pickPriorityTopic: %v", err)
		}
		if topic != "" {
			t.Fatalf("expected no priority topic, got %q", topic)
		}
	})

	t.Run("invalid values in weak_topics are skipped, not returned", func(t *testing.T) {
		// weak_topics is normally only ever written pre-validated (see
		// extractWeakTopics), but this covers the defense-in-depth
		// re-validation added for a row that got here some other way
		// (a raw DB edit, an older schema, anything).
		repo.weakZones[telegramID] = nil
		session := &db.Session{WeakTopics: "кулинария,бд"}

		topic, err := h.pickPriorityTopic(ctx, student, session)
		if err != nil {
			t.Fatalf("pickPriorityTopic: %v", err)
		}
		if topic != "бд" {
			t.Fatalf("expected the invalid topic to be skipped and \"бд\" returned, got %q", topic)
		}
	})

	t.Run("weak_topics containing only invalid values yields no priority topic", func(t *testing.T) {
		repo.weakZones[telegramID] = nil
		session := &db.Session{WeakTopics: "кулинария,астрология"}

		topic, err := h.pickPriorityTopic(ctx, student, session)
		if err != nil {
			t.Fatalf("pickPriorityTopic: %v", err)
		}
		if topic != "" {
			t.Fatalf("expected no priority topic when nothing in weak_topics is valid, got %q", topic)
		}
	})
}

func TestEvaluateAnswer_NilCurrentQuestionIDDoesNotPanic(t *testing.T) {
	repo := newFakeRepo()
	logger, logs := newCapturingLogger()
	h := New(repo, &fakeLLM{}, logger, 8, nil)

	student := &db.Student{TelegramID: 88}
	session := &db.Session{ID: 1, StudentID: 88, CurrentQuestionID: nil} // the invariant evaluateAnswer relies on, deliberately violated

	ctx := newCtx(88, "какой-то ответ")

	// Must not panic: evaluateAnswer's own guard should catch this
	// before reaching the *session.CurrentQuestionID dereference.
	if err := h.evaluateAnswer(context.Background(), ctx, student, session); err != nil {
		t.Fatalf("evaluateAnswer: %v", err)
	}

	if len(ctx.sent) != 1 || ctx.sent[0] != genericErrorMessage {
		t.Fatalf("expected the friendly error message, got %v", ctx.sent)
	}
	if !strings.Contains(logs.String(), "evaluateAnswer called with no current question") {
		t.Fatalf("expected the guard to log what happened, got:\n%s", logs.String())
	}
}

func TestRecoverMiddleware_CatchesPanicAndLogsFully(t *testing.T) {
	repo := newFakeRepo()
	logger, logs := newCapturingLogger()
	h := New(repo, &fakeLLM{}, logger, 8, nil)

	panicking := func(tele.Context) error {
		panic("boom: nil pointer somewhere")
	}
	wrapped := h.recoverMiddleware(panicking)

	panicCtx := newCtx(42, "какой-то текст")

	// The whole point: this must not panic out of the call.
	if err := wrapped(panicCtx); err != nil {
		t.Fatalf("expected the recovered handler to return nil, got %v", err)
	}

	if len(panicCtx.sent) != 1 || panicCtx.sent[0] != genericErrorMessage {
		t.Fatalf("expected the friendly error message to be sent, got %v", panicCtx.sent)
	}

	logText := logs.String()
	for _, want := range []string{
		"panic in telegram handler",
		"boom: nil pointer somewhere",
		"telegram_id=42",
		"stack=",
		"fsm_status=not_registered",
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("expected the panic log to contain %q, got:\n%s", want, logText)
		}
	}
}

// TestRecoverMiddleware_SurvivesPanicWhilePanicHandling is the
// regression test for a real bug found while building
// TestRegister_AllEndpointsWrappedByRecovery: the original
// recoverMiddleware called h.fsmStatusForLogging directly from inside
// its own recover to enrich the log entry. fsmStatusForLogging calls
// the repository; when the repository is broken badly enough to panic
// the original handler, that same brokenness panicked this "recovery"
// call too, and that second panic had nothing left to catch it,
// crashing the process the whole mechanism exists to protect. This
// pins down the fix: the panic-handling path must survive the
// repository being broken, not just the handler being buggy.
func TestRecoverMiddleware_SurvivesPanicWhilePanicHandling(t *testing.T) {
	repo := newFakeRepo()
	repo.panicOnAnyCall = true
	logger, logs := newCapturingLogger()
	h := New(repo, &fakeLLM{}, logger, 8, nil)

	panicking := func(tele.Context) error {
		panic("boom: something in the real handler broke")
	}
	wrapped := h.recoverMiddleware(panicking)

	ctx := newCtx(42, "текст")

	if err := wrapped(ctx); err != nil {
		t.Fatalf("expected the recovered handler to return nil even though the repository also panics, got %v", err)
	}
	if len(ctx.sent) != 1 || ctx.sent[0] != genericErrorMessage {
		t.Fatalf("expected the friendly error message despite the repository also panicking, got %v", ctx.sent)
	}

	logText := logs.String()
	if !strings.Contains(logText, "panic in telegram handler") {
		t.Fatalf("expected the original panic to still be logged, got:\n%s", logText)
	}
	if !strings.Contains(logText, "lookup_panicked") {
		t.Fatalf("expected fsm_status to record that its own lookup panicked, got:\n%s", logText)
	}
}

func TestOnError_LogsFullDetailAndSendsFriendlyMessage(t *testing.T) {
	repo := newFakeRepo("SA2026-TEST")
	logger, logs := newCapturingLogger()
	h := New(repo, &fakeLLM{}, logger, 8, nil)

	const telegramID = 77
	ctx := context.Background()

	student, err := repo.CreateStudentIfAccessCodeValid(ctx, telegramID, "Тест", "SA2026-TEST")
	if err != nil {
		t.Fatalf("seed student: %v", err)
	}
	session, err := repo.StartSession(ctx, student.TelegramID)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := repo.AdvancePhase(ctx, session.ID, db.SessionStatusQuestionCycle); err != nil {
		t.Fatalf("advance phase: %v", err)
	}

	errCtx := newCtx(telegramID, "ответ студента")
	h.OnError(errors.New("telegram: bad request: chat not found"), errCtx)

	if len(errCtx.sent) != 1 || errCtx.sent[0] != genericErrorMessage {
		t.Fatalf("expected the friendly error message to be sent, got %v", errCtx.sent)
	}

	logText := logs.String()
	for _, want := range []string{
		"telegram handler returned an error",
		"chat not found",
		"telegram_id=77",
		"fsm_status=QUESTION_CYCLE",
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("expected the error log to contain %q, got:\n%s", want, logText)
		}
	}
}

func TestOnError_NilContextDoesNotPanic(t *testing.T) {
	logger, logs := newCapturingLogger()
	h := New(newFakeRepo(), &fakeLLM{}, logger, 8, nil)

	h.OnError(errors.New("some transport-level error"), nil)

	if !strings.Contains(logs.String(), "some transport-level error") {
		t.Errorf("expected the error to still be logged with a nil context, got:\n%s", logs.String())
	}
}

func TestFsmStatusForLogging(t *testing.T) {
	repo := newFakeRepo("SA2026-TEST")
	h := New(repo, &fakeLLM{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 8, nil)
	ctx := context.Background()

	if got := h.fsmStatusForLogging(ctx, 0); got != "unknown (no sender)" {
		t.Errorf("telegramID=0: got %q", got)
	}
	if got := h.fsmStatusForLogging(ctx, 12345); got != "not_registered" {
		t.Errorf("unregistered: got %q", got)
	}

	student, err := repo.CreateStudentIfAccessCodeValid(ctx, 12345, "Тест", "SA2026-TEST")
	if err != nil {
		t.Fatalf("seed student: %v", err)
	}
	if got := h.fsmStatusForLogging(ctx, student.TelegramID); got != "no_active_session" {
		t.Errorf("no session: got %q", got)
	}

	session, err := repo.StartSession(ctx, student.TelegramID)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if got := h.fsmStatusForLogging(ctx, student.TelegramID); got != db.SessionStatusInstruction {
		t.Errorf("fresh session: got %q, want %q", got, db.SessionStatusInstruction)
	}

	if err := repo.AdvancePhase(ctx, session.ID, db.SessionStatusQuestionCycle); err != nil {
		t.Fatalf("advance phase: %v", err)
	}
	if got := h.fsmStatusForLogging(ctx, student.TelegramID); got != db.SessionStatusQuestionCycle {
		t.Errorf("after advance: got %q, want %q", got, db.SessionStatusQuestionCycle)
	}
}

// TestRegister_AllEndpointsWrappedByRecovery is the regression test for
// the "confirmed hole": a callback handler (or any handler) registered
// without going through recoverMiddleware. It does not hand-wrap
// anything itself, unlike the other panic tests in this file: it builds
// a real *tele.Bot, calls the actual Register(), and dispatches through
// Bot.Trigger, which looks up the handler from the exact same map
// Bot.Handle populates at real update-processing time. If a future
// endpoint is ever added to Register via a bare bot.Handle instead of
// h.handle, this test starts failing for that one endpoint.
func TestRegister_AllEndpointsWrappedByRecovery(t *testing.T) {
	const telegramID = 555

	endpoints := []struct {
		name     string
		endpoint interface{}
	}{
		{"/start command", "/start"},
		{"/report command", "/report"},
		{"restart callback", &btnRestart},
		{"continue callback", &btnContinue},
		{"text message", tele.OnText},
	}

	for _, tc := range endpoints {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			repo.panicOnAnyCall = true
			logger, logs := newCapturingLogger()
			// telegramID is also made an admin so the /report case
			// reaches the repository too, instead of returning early on
			// the non-admin silent-ignore path.
			h := New(repo, &fakeLLM{}, logger, 8, []int64{telegramID})

			bot, err := tele.NewBot(tele.Settings{Offline: true, OnError: h.OnError})
			if err != nil {
				t.Fatalf("NewBot: %v", err)
			}
			h.Register(bot)

			ctx := newCtx(telegramID, "какой-то текст", "какой-то текст")

			// The real assertion: Trigger must not propagate a panic or
			// even a returned error. If this endpoint were registered
			// without recoverMiddleware, the panic from fakeRepo would
			// blow straight through Trigger and fail this test (or, in
			// production, take down the whole process).
			if triggerErr := bot.Trigger(tc.endpoint, ctx); triggerErr != nil {
				t.Fatalf("Trigger returned an error instead of the panic being recovered: %v", triggerErr)
			}

			if !strings.Contains(logs.String(), "panic in telegram handler") {
				t.Fatalf("expected the panic to be recovered and logged, got log:\n%s\nsent to student: %v",
					logs.String(), ctx.sent)
			}
			if len(ctx.sent) == 0 || ctx.sent[len(ctx.sent)-1] != genericErrorMessage {
				t.Fatalf("expected the friendly error message to be sent, got %v", ctx.sent)
			}
		})
	}
}
