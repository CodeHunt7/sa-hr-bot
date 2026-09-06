package handlers

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
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
	data   string

	sent         []string
	sentEvents   []string
	sentOptions  [][]interface{}
	sentPhotos   []*tele.Photo
	sentVideos   []*tele.Video
	sentDocs     []*tele.Document
	sentDocBytes [][]byte
	responded    bool
}

func (f *fakeContext) Sender() *tele.User { return f.sender }
func (f *fakeContext) Text() string       { return f.text }
func (f *fakeContext) Args() []string     { return f.args }
func (f *fakeContext) Data() string       { return f.data }

func (f *fakeContext) Send(what interface{}, options ...interface{}) error {
	f.sentOptions = append(f.sentOptions, append([]interface{}(nil), options...))
	switch v := what.(type) {
	case string:
		f.sent = append(f.sent, v)
		f.sentEvents = append(f.sentEvents, "text")
	case *tele.Photo:
		f.sentPhotos = append(f.sentPhotos, v)
		f.sentEvents = append(f.sentEvents, "photo")
	case *tele.Video:
		f.sentVideos = append(f.sentVideos, v)
		f.sentEvents = append(f.sentEvents, "video")
	case *tele.Document:
		f.sentDocs = append(f.sentDocs, v)
		f.sentEvents = append(f.sentEvents, "document")
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
	students      map[int64]*db.Student
	sessions      map[int64]*db.Session
	nextID        int64
	codes         map[string]bool // code -> used
	summaries     []db.SessionSummary
	weakZones     map[int64][]db.WeakZone
	questions     map[int64]*db.QuestionBank
	attempts      map[int64]*db.QuestionAttempt
	nextAttemptID int64
	tokenUsage    map[string]*db.TokenUsageReport

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
		students:   make(map[int64]*db.Student),
		sessions:   make(map[int64]*db.Session),
		codes:      m,
		weakZones:  make(map[int64][]db.WeakZone),
		questions:  make(map[int64]*db.QuestionBank),
		attempts:   make(map[int64]*db.QuestionAttempt),
		tokenUsage: make(map[string]*db.TokenUsageReport),
	}
}

func (r *fakeRepo) CreateAccessCodes(_ context.Context, count int) ([]string, error) {
	r.maybePanic()
	if count < 1 || count > 100 {
		return nil, errors.New("invalid count")
	}
	result := make([]string, 0, count)
	for len(result) < count {
		code := fmt.Sprintf("SA2026-A%03d", len(r.codes)+1)
		if _, exists := r.codes[code]; exists {
			continue
		}
		r.codes[code] = false
		result = append(result, code)
	}
	return result, nil
}

func (r *fakeRepo) DeleteUnusedAccessCode(_ context.Context, code string) error {
	r.maybePanic()
	used, ok := r.codes[code]
	if !ok {
		return db.ErrAccessCodeNotFound
	}
	if used {
		return db.ErrAccessCodeInUse
	}
	delete(r.codes, code)
	return nil
}

func (r *fakeRepo) SaveLLMUsage(_ context.Context, studentID, _ int64, _ string, promptTokens, completionTokens, totalTokens, cachedTokens int64) error {
	r.maybePanic()
	student, ok := r.students[studentID]
	if !ok {
		// Some focused handler tests pass an already loaded student directly
		// without seeding registration state. The real DB always has the FK;
		// those tests only need usage persistence not to obscure their subject.
		return nil
	}
	report := r.tokenUsage[student.AccessCode]
	if report == nil {
		id := student.TelegramID
		report = &db.TokenUsageReport{Code: student.AccessCode, IsUsed: true, StudentID: &id, StudentName: student.Name}
		r.tokenUsage[student.AccessCode] = report
	}
	report.Calls++
	report.PromptTokens += promptTokens
	report.CompletionTokens += completionTokens
	report.TotalTokens += totalTokens
	report.CachedTokens += cachedTokens
	return nil
}

func (r *fakeRepo) GetTokenUsageByAccessCode(_ context.Context, code string) (*db.TokenUsageReport, error) {
	r.maybePanic()
	used, ok := r.codes[code]
	if !ok {
		return nil, db.ErrAccessCodeNotFound
	}
	if report := r.tokenUsage[code]; report != nil {
		cp := *report
		return &cp, nil
	}
	return &db.TokenUsageReport{Code: code, IsUsed: used}, nil
}

func (r *fakeRepo) ListAccessCodes(_ context.Context) ([]db.TokenUsageReport, error) {
	r.maybePanic()
	reports := make([]db.TokenUsageReport, 0, len(r.codes))
	for code, used := range r.codes {
		report := db.TokenUsageReport{Code: code, IsUsed: used}
		if saved := r.tokenUsage[code]; saved != nil {
			report = *saved
		} else if used {
			for _, student := range r.students {
				if student.AccessCode == code {
					id := student.TelegramID
					report.StudentID = &id
					report.StudentName = student.Name
					break
				}
			}
		}
		reports = append(reports, report)
	}
	sort.Slice(reports, func(i, j int) bool {
		if reports[i].IsUsed != reports[j].IsUsed {
			return !reports[i].IsUsed
		}
		return reports[i].Code < reports[j].Code
	})
	return reports, nil
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
	if status != db.SessionStatusCompleted && status != db.SessionStatusAbandoned {
		return errors.New("invalid terminal status")
	}
	s, ok := r.sessions[sessionID]
	if !ok || s.EndedAt != nil {
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

func (r *fakeRepo) GetQuestionByID(_ context.Context, id int64) (*db.QuestionBank, error) {
	r.maybePanic()
	q, ok := r.questions[id]
	if !ok {
		return nil, db.ErrQuestionNotFound
	}
	return q, nil
}

func (r *fakeRepo) SetQualificationAnswer(_ context.Context, sessionID int64, step int, answer string) error {
	r.maybePanic()
	s, ok := r.sessions[sessionID]
	if !ok {
		return errors.New("session not found")
	}
	if s.QualificationStep != step {
		return errors.New("unexpected qualification step")
	}
	switch step {
	case db.QualificationStepCurrentGrade:
		s.CurrentGrade = answer
	case db.QualificationStepTargetGrade:
		s.Grade = answer
	case db.QualificationStepStrongZones:
		s.StrongZones = answer
	case db.QualificationStepWeakZones:
		s.WeakZonesInput = answer
	default:
		return errors.New("invalid qualification step")
	}
	s.QualificationStep++
	return nil
}

func (r *fakeRepo) UpdateLegacyTargetGrade(_ context.Context, sessionID int64, grade string) error {
	r.maybePanic()
	s, ok := r.sessions[sessionID]
	if !ok || s.EndedAt != nil || s.Status != db.SessionStatusQuestionCycle || s.Grade != "джун" {
		return errors.New("session is not waiting for a replacement target grade")
	}
	if grade != "мидл" && grade != "сеньор" {
		return errors.New("unsupported target grade")
	}
	s.Grade = grade
	return nil
}

func (r *fakeRepo) ResetQualification(_ context.Context, sessionID int64) error {
	r.maybePanic()
	s, ok := r.sessions[sessionID]
	if !ok || s.Status != db.SessionStatusProfileConfirmation {
		return errors.New("session is not waiting for confirmation")
	}
	s.Status = db.SessionStatusQualification
	s.QualificationStep = db.QualificationStepCurrentGrade
	s.CurrentGrade = ""
	s.Grade = ""
	s.StrongZones = ""
	s.WeakZonesInput = ""
	s.WeakTopics = ""
	return nil
}

func (r *fakeRepo) ConfirmQualification(_ context.Context, sessionID, studentID int64, topics []string) error {
	r.maybePanic()
	s, ok := r.sessions[sessionID]
	if !ok || s.Status != db.SessionStatusProfileConfirmation || s.QualificationStep != db.QualificationStepDone {
		return errors.New("session is not waiting for confirmation")
	}
	s.Status = db.SessionStatusKDIRLesson
	s.WeakTopics = strings.Join(topics, ",")
	for _, topic := range topics {
		_, _ = r.SaveWeakZone(context.Background(), studentID, topic, db.WeakZoneStatusHypothesis)
	}
	return nil
}

func (r *fakeRepo) SaveAuditResults(_ context.Context, sessionID, studentID int64, topics []string) error {
	r.maybePanic()
	s, ok := r.sessions[sessionID]
	if !ok {
		return errors.New("session not found")
	}
	s.WeakTopics = strings.Join(topics, ",")
	for _, topic := range topics {
		_, _ = r.SaveWeakZone(context.Background(), studentID, topic, db.WeakZoneStatusHypothesis)
	}
	return nil
}

func (r *fakeRepo) GetWeakZones(_ context.Context, studentID int64) ([]db.WeakZone, error) {
	r.maybePanic()
	return r.weakZones[studentID], nil
}

func (r *fakeRepo) SaveWeakZone(_ context.Context, studentID int64, zoneText, status string) (*db.WeakZone, error) {
	r.maybePanic()
	for i := range r.weakZones[studentID] {
		if r.weakZones[studentID][i].ZoneText == zoneText {
			r.weakZones[studentID][i].Status = status
			r.weakZones[studentID][i].UpdatedAt = time.Now()
			zone := r.weakZones[studentID][i]
			return &zone, nil
		}
	}
	zone := db.WeakZone{
		ID: int64(len(r.weakZones[studentID])) + 1, StudentID: studentID,
		ZoneText: zoneText, Status: status, UpdatedAt: time.Now(),
	}
	r.weakZones[studentID] = append(r.weakZones[studentID], zone)
	return &zone, nil
}

func (r *fakeRepo) StartQuestionAttempt(_ context.Context, sessionID, questionID int64) (*db.QuestionAttempt, error) {
	r.maybePanic()
	for _, attempt := range r.attempts {
		if attempt.SessionID == sessionID && attempt.Status != db.QuestionAttemptCompleted {
			return nil, errors.New("active attempt already exists")
		}
	}
	session, ok := r.sessions[sessionID]
	if !ok {
		return nil, errors.New("session not found")
	}
	r.nextAttemptID++
	now := time.Now()
	attempt := &db.QuestionAttempt{
		ID: r.nextAttemptID, SessionID: sessionID, QuestionID: questionID,
		Status: db.QuestionAttemptWaitingPrimary, CreatedAt: now, UpdatedAt: now,
	}
	r.attempts[attempt.ID] = attempt
	session.CurrentQuestionID = &questionID
	session.NextTopic = ""
	cp := *attempt
	return &cp, nil
}

func (r *fakeRepo) GetActiveQuestionAttempt(_ context.Context, sessionID int64) (*db.QuestionAttempt, error) {
	r.maybePanic()
	for _, attempt := range r.attempts {
		if attempt.SessionID == sessionID && attempt.Status != db.QuestionAttemptCompleted {
			cp := *attempt
			return &cp, nil
		}
	}
	return nil, db.ErrNoActiveQuestionAttempt
}

func (r *fakeRepo) SavePrimaryFeedback(_ context.Context, attemptID int64, answer, feedback, followupQuestion string) error {
	r.maybePanic()
	attempt, ok := r.attempts[attemptID]
	if !ok || attempt.Status != db.QuestionAttemptWaitingPrimary {
		return errors.New("attempt is not waiting for primary")
	}
	attempt.PrimaryAnswer = answer
	attempt.PrimaryFeedback = feedback
	attempt.FollowupQuestion = followupQuestion
	attempt.Status = db.QuestionAttemptPrimaryFeedbackReady
	attempt.UpdatedAt = time.Now()
	return nil
}

func (r *fakeRepo) MarkPrimaryFeedbackDelivered(_ context.Context, attemptID int64) error {
	r.maybePanic()
	attempt, ok := r.attempts[attemptID]
	if !ok || attempt.Status != db.QuestionAttemptPrimaryFeedbackReady {
		return errors.New("primary feedback is not ready")
	}
	attempt.Status = db.QuestionAttemptWaitingFollowup
	return nil
}

func (r *fakeRepo) SaveFollowupFeedback(_ context.Context, attemptID int64, answer, feedback, followup2Question string) error {
	r.maybePanic()
	attempt, ok := r.attempts[attemptID]
	if !ok || attempt.Status != db.QuestionAttemptWaitingFollowup {
		return errors.New("attempt is not waiting for followup")
	}
	attempt.FollowupAnswer = answer
	attempt.FollowupFeedback = feedback
	attempt.Followup2Question = followup2Question
	attempt.Status = db.QuestionAttemptFollowupFeedbackReady
	attempt.UpdatedAt = time.Now()
	return nil
}

func (r *fakeRepo) MarkFollowupFeedbackDelivered(_ context.Context, attemptID int64) error {
	r.maybePanic()
	attempt, ok := r.attempts[attemptID]
	if !ok || attempt.Status != db.QuestionAttemptFollowupFeedbackReady {
		return errors.New("followup feedback is not ready")
	}
	attempt.Status = db.QuestionAttemptWaitingFollowup2
	return nil
}

func (r *fakeRepo) FinalizeQuestionAttempt(_ context.Context, sessionID, studentID, attemptID int64, answer, miniFeedback, fullFeedback, zoneTopic, zoneStatus string) (int, error) {
	r.maybePanic()
	attempt, ok := r.attempts[attemptID]
	if !ok || attempt.Status != db.QuestionAttemptWaitingFollowup2 {
		return 0, errors.New("attempt is not waiting for followup 2")
	}
	session, ok := r.sessions[sessionID]
	if !ok {
		return 0, errors.New("session not found")
	}
	attempt.Followup2Answer = answer
	attempt.Followup2Feedback = miniFeedback
	attempt.FinalFeedback = fullFeedback
	attempt.Status = db.QuestionAttemptFinalFeedbackReady
	attempt.UpdatedAt = time.Now()
	if zoneTopic != "" {
		_, _ = r.SaveWeakZone(context.Background(), studentID, zoneTopic, zoneStatus)
	}
	session.CycleCount++
	return session.CycleCount, nil
}

func (r *fakeRepo) MarkFinalFeedbackDelivered(_ context.Context, attemptID int64) error {
	r.maybePanic()
	attempt, ok := r.attempts[attemptID]
	if !ok || attempt.Status != db.QuestionAttemptFinalFeedbackReady {
		return errors.New("final feedback is not ready")
	}
	attempt.Status = db.QuestionAttemptWaitingVector
	return nil
}

func (r *fakeRepo) CompleteQuestionAttempt(_ context.Context, sessionID, attemptID int64, selectedVector, nextTopic string) error {
	r.maybePanic()
	attempt, ok := r.attempts[attemptID]
	if !ok || attempt.Status != db.QuestionAttemptWaitingVector {
		return errors.New("attempt is not waiting for vector")
	}
	session, ok := r.sessions[sessionID]
	if !ok {
		return errors.New("session not found")
	}
	attempt.SelectedVector = selectedVector
	attempt.Status = db.QuestionAttemptCompleted
	attempt.UpdatedAt = time.Now()
	session.CurrentQuestionID = nil
	session.NextTopic = nextTopic
	return nil
}

func (r *fakeRepo) ResumeQuestionCycle(_ context.Context, sessionID int64) error {
	r.maybePanic()
	session, ok := r.sessions[sessionID]
	if !ok || session.Status != db.SessionStatusSummary || session.EndedAt != nil {
		return errors.New("session is not waiting at summary")
	}
	session.Status = db.SessionStatusQuestionCycle
	for i := len(r.summaries) - 1; i >= 0; i-- {
		if r.summaries[i].SessionID == sessionID {
			r.summaries = append(r.summaries[:i], r.summaries[i+1:]...)
		}
	}
	return nil
}

func (r *fakeRepo) GetSessionAttemptReports(_ context.Context, sessionID int64) ([]db.QuestionAttemptReport, error) {
	r.maybePanic()
	var result []db.QuestionAttemptReport
	for _, attempt := range r.attempts {
		if attempt.SessionID != sessionID || (attempt.Status != db.QuestionAttemptCompleted && attempt.Status != db.QuestionAttemptWaitingVector) {
			continue
		}
		question := r.questions[attempt.QuestionID]
		result = append(result, db.QuestionAttemptReport{
			QuestionText: question.QuestionText, Topic: question.Topic,
			PrimaryAnswer: attempt.PrimaryAnswer, FollowupQuestion: attempt.FollowupQuestion,
			FollowupAnswer: attempt.FollowupAnswer, Followup2Question: attempt.Followup2Question,
			Followup2Answer: attempt.Followup2Answer, FinalFeedback: attempt.FinalFeedback,
		})
	}
	return result, nil
}

func (r *fakeRepo) SaveSummary(_ context.Context, sessionID int64, summaryText string) (*db.SessionSummary, error) {
	r.maybePanic()
	for i := range r.summaries {
		if r.summaries[i].SessionID == sessionID {
			r.summaries[i].SummaryText = summaryText
			return &r.summaries[i], nil
		}
	}
	sum := db.SessionSummary{ID: int64(len(r.summaries)) + 1, SessionID: sessionID, SummaryText: summaryText}
	r.summaries = append(r.summaries, sum)
	return &sum, nil
}

func (r *fakeRepo) GetSummaryBySessionID(_ context.Context, sessionID int64) (*db.SessionSummary, error) {
	r.maybePanic()
	for i := range r.summaries {
		if r.summaries[i].SessionID == sessionID {
			return &r.summaries[i], nil
		}
	}
	return nil, db.ErrSummaryNotFound
}

// GetStudentReports derives the same activity and token aggregate the real
// repository computes in SQL, from the fake's in-memory state.
func (r *fakeRepo) GetStudentReports(_ context.Context) ([]db.StudentReport, error) {
	r.maybePanic()
	reports := make([]db.StudentReport, 0, len(r.students))
	for id, s := range r.students {
		rep := db.StudentReport{TelegramID: id, Name: s.Name, AccessCode: s.AccessCode}
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
		if usage := r.tokenUsage[s.AccessCode]; usage != nil {
			rep.Calls = usage.Calls
			rep.PromptTokens = usage.PromptTokens
			rep.CompletionTokens = usage.CompletionTokens
			rep.TotalTokens = usage.TotalTokens
			rep.CachedTokens = usage.CachedTokens
		}
		reports = append(reports, rep)
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].TelegramID < reports[j].TelegramID })
	return reports, nil
}

// fakeLLM is an in-memory stand-in for *llm.Service.
type fakeLLM struct {
	replies           []string
	question          *db.QuestionBank
	pickCalls         int
	replyCalls        int
	evalCalls         int
	followupEvalCalls int
	blockEvalCalls    int
	pickErr           error // if set, returned by the next PickQuestion call, then cleared
	usage             llm.Usage

	lastEvalQuestion             *db.QuestionBank
	lastPickGrade, lastPickTopic string
	lastReplyContext             string
}

func (f *fakeLLM) nextReply() *llm.Reply {
	if len(f.replies) == 0 {
		return &llm.Reply{Text: "ok", Usage: f.usage}
	}
	text := f.replies[0]
	f.replies = f.replies[1:]
	return &llm.Reply{Text: text, Usage: f.usage}
}

func (f *fakeLLM) Reply(_ context.Context, userMessage string) (*llm.Reply, error) {
	f.replyCalls++
	f.lastReplyContext = userMessage
	return f.nextReply(), nil
}

func TestRunSummaryReusesSavedReportAfterSendFailure(t *testing.T) {
	repo := newFakeRepo()
	student := &db.Student{TelegramID: 77, Name: "Тест", AccessCode: "SA2026-TEST"}
	repo.students[student.TelegramID] = student
	session := &db.Session{
		ID: 1, StudentID: student.TelegramID, Status: db.SessionStatusSummary,
		StartedAt: time.Now(),
	}
	repo.sessions[session.ID] = session
	repo.summaries = []db.SessionSummary{{
		ID: 1, SessionID: session.ID, SummaryText: "Уже сохраненный итог",
	}}
	lm := &fakeLLM{replies: []string{"не должен использоваться"}}
	h := New(repo, lm, slog.New(slog.NewTextHandler(io.Discard, nil)), 8, nil)
	ctx := newCtx(student.TelegramID, "повтор")

	if err := h.runSummary(context.Background(), ctx, student, session); err != nil {
		t.Fatalf("runSummary: %v", err)
	}
	if lm.replyCalls != 0 {
		t.Fatalf("saved summary should avoid another LLM call, got %d calls", lm.replyCalls)
	}
	if len(ctx.sent) != 1 || !strings.Contains(ctx.sent[0], "Уже сохраненный итог") || !strings.Contains(ctx.sent[0], "/start SA2026-TEST") {
		t.Fatalf("unexpected sent messages: %v", ctx.sent)
	}
	if repo.sessions[session.ID].Status != db.SessionStatusSummary || repo.sessions[session.ID].EndedAt != nil {
		t.Fatalf("summary must keep session active: %+v", repo.sessions[session.ID])
	}
}

func (f *fakeLLM) Evaluate(_ context.Context, _ llm.StudentProfile, _ []db.WeakZone, _ string, question *db.QuestionBank) (*llm.Reply, error) {
	f.evalCalls++
	f.lastEvalQuestion = question
	return f.nextReply(), nil
}

func (f *fakeLLM) EvaluateFollowup(_ context.Context, _ llm.StudentProfile, _ []db.WeakZone, _, _, _ string, question *db.QuestionBank) (*llm.Reply, error) {
	f.followupEvalCalls++
	f.lastEvalQuestion = question
	return f.nextReply(), nil
}

func (f *fakeLLM) EvaluateBlock(_ context.Context, _ llm.StudentProfile, _ []db.WeakZone, _, _, _, _, _ string, question *db.QuestionBank) (*llm.Reply, error) {
	f.blockEvalCalls++
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

func (f *fakeLLM) PickQuestionForSession(ctx context.Context, _ int64, grade, topic string) (*db.QuestionBank, error) {
	return f.PickQuestion(ctx, grade, topic)
}

func TestFullSessionFlow(t *testing.T) {
	repo := newFakeRepo("SA2026-TEST")
	question := &db.QuestionBank{
		ID: 1, QuestionText: "q", Grade: "мидл", Topic: "бд",
		Followup1: "follow-up q", Followup2: "follow-up q2",
		AnswerJunior: "junior answer", AnswerMiddle: "middle answer", AnswerSenior: "senior answer",
	}
	repo.questions[question.ID] = question
	lm := &fakeLLM{question: question, usage: llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CachedTokens: 3}}
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

	// Valid code: registers, sends the welcome photo, fixed instruction and
	// current-grade question, then lands in deterministic QUALIFICATION without
	// calling the model.
	startCtx := newCtx(telegramID, "/start SA2026-TEST", "SA2026-TEST")
	if err := h.handleStart(startCtx); err != nil {
		t.Fatalf("handleStart: %v", err)
	}
	if len(startCtx.sentPhotos) != 1 || startCtx.sentPhotos[0].FileLocal != welcomePhotoPath {
		t.Fatalf("expected welcome photo, got %+v", startCtx.sentPhotos)
	}
	if len(startCtx.sent) != 2 || startCtx.sent[0] != instructionMessage || startCtx.sent[1] != currentGradeQuestion {
		t.Fatalf("unexpected initial messages: %v", startCtx.sent)
	}
	session, err := repo.GetActiveSession(ctx, telegramID)
	if err != nil {
		t.Fatalf("GetActiveSession: %v", err)
	}
	if session.Status != db.SessionStatusQualification {
		t.Fatalf("expected QUALIFICATION after instruction, got %s", session.Status)
	}

	// Current and target grades are chosen explicitly, then strong and weak
	// zones are stored verbatim. No reply markers or model calls are involved.
	currentGradeCtx := newCtx(telegramID, "")
	currentGradeCtx.data = "джун"
	if err := h.handleGradeCallback(currentGradeCtx); err != nil {
		t.Fatalf("handle current grade callback: %v", err)
	}
	if len(currentGradeCtx.sent) != 1 || currentGradeCtx.sent[0] != targetGradeQuestion {
		t.Fatalf("expected target grade question, got %v", currentGradeCtx.sent)
	}

	rejectedJuniorTarget := newCtx(telegramID, "")
	rejectedJuniorTarget.data = "джун"
	if err := h.handleGradeCallback(rejectedJuniorTarget); err != nil {
		t.Fatalf("reject junior target grade callback: %v", err)
	}
	if len(rejectedJuniorTarget.sent) != 1 || !strings.Contains(rejectedJuniorTarget.sent[0], "мидл или сеньор") {
		t.Fatalf("expected target-grade restriction, got %v", rejectedJuniorTarget.sent)
	}
	session, _ = repo.GetActiveSession(ctx, telegramID)
	if session.QualificationStep != db.QualificationStepTargetGrade || session.Grade != "" {
		t.Fatalf("junior target grade must not be saved, got %+v", session)
	}

	targetGradeCtx := newCtx(telegramID, "")
	targetGradeCtx.data = "мидл"
	if err := h.handleGradeCallback(targetGradeCtx); err != nil {
		t.Fatalf("handle target grade callback: %v", err)
	}
	if len(targetGradeCtx.sent) != 1 || targetGradeCtx.sent[0] != strongZonesQuestion {
		t.Fatalf("expected strong zones question, got %v", targetGradeCtx.sent)
	}
	if !strings.Contains(targetGradeCtx.sent[0], "<i>выбери варианты и напиши мне их текстом</i>") ||
		len(targetGradeCtx.sentOptions) != 1 || len(targetGradeCtx.sentOptions[0]) != 1 || targetGradeCtx.sentOptions[0][0] != tele.ModeHTML {
		t.Fatalf("expected italic hint rendered as Telegram HTML, text=%q options=%+v", targetGradeCtx.sent[0], targetGradeCtx.sentOptions)
	}

	strongCtx := newCtx(telegramID, "Требования и архитектура")
	if err := h.handleMessage(strongCtx); err != nil {
		t.Fatalf("strong zones answer: %v", err)
	}
	if len(strongCtx.sent) != 1 || strongCtx.sent[0] != weakZonesQuestion {
		t.Fatalf("expected weak zones question, got %v", strongCtx.sent)
	}
	if !strings.Contains(strongCtx.sent[0], "<i>выбери варианты и напиши мне их текстом</i>") ||
		len(strongCtx.sentOptions) != 1 || len(strongCtx.sentOptions[0]) != 1 || strongCtx.sentOptions[0][0] != tele.ModeHTML {
		t.Fatalf("expected italic hint rendered as Telegram HTML, text=%q options=%+v", strongCtx.sent[0], strongCtx.sentOptions)
	}

	session, _ = repo.GetActiveSession(ctx, telegramID)
	if session.Status != db.SessionStatusQualification || session.QualificationStep != db.QualificationStepWeakZones {
		t.Fatalf("expected QUALIFICATION at weak zones step, got status=%s step=%d", session.Status, session.QualificationStep)
	}
	if session.CurrentGrade != "джун" || session.Grade != "мидл" || session.StrongZones == "" {
		t.Fatalf("expected first 3 qualification answers captured, got %+v", session)
	}

	// The fourth answer shows a deterministic profile confirmation. The old
	// LLM mini-audit is not called.
	qualDoneCtx := newCtx(telegramID, "Интеграции и Базы данных")
	if err := h.handleMessage(qualDoneCtx); err != nil {
		t.Fatalf("weak zones answer: %v", err)
	}
	if len(qualDoneCtx.sent) != 0 || len(qualDoneCtx.sentPhotos) != 1 || qualDoneCtx.sentPhotos[0].FileLocal != readyPhotoPath ||
		!strings.Contains(qualDoneCtx.sentPhotos[0].Caption, "Текущий грейд: Джун") ||
		!strings.Contains(qualDoneCtx.sentPhotos[0].Caption, "Интеграции и Базы данных") {
		t.Fatalf("expected profile confirmation attached to pic2, got text=%v photos=%+v", qualDoneCtx.sent, qualDoneCtx.sentPhotos)
	}
	session, _ = repo.GetActiveSession(ctx, telegramID)
	if session.Status != db.SessionStatusProfileConfirmation || session.QualificationStep != db.QualificationStepDone {
		t.Fatalf("expected profile confirmation state, got %+v", session)
	}
	if lm.replyCalls != 0 || lm.pickCalls != 0 {
		t.Fatalf("qualification and confirmation must not call LLM or pick a question, reply=%d pick=%d", lm.replyCalls, lm.pickCalls)
	}

	confirmCtx := newCtx(telegramID, "")
	confirmCtx.data = "confirm"
	if err := h.handleConfirmProfileCallback(confirmCtx); err != nil {
		t.Fatalf("confirm profile: %v", err)
	}
	if len(confirmCtx.sent) != 1 || confirmCtx.sent[0] != kdirLessonMessage {
		t.Fatalf("expected KDIR lesson with ready button, got %v", confirmCtx.sent)
	}
	if len(confirmCtx.sentPhotos) != 0 {
		t.Fatalf("pic2 must be attached to profile confirmation, not sent after the lesson: %+v", confirmCtx.sentPhotos)
	}
	if len(confirmCtx.sentVideos) != 1 || confirmCtx.sentVideos[0].FileLocal != kdirVideoPath || !confirmCtx.sentVideos[0].Streaming {
		t.Fatalf("expected local streaming KDIR video, got %+v", confirmCtx.sentVideos)
	}
	if got := strings.Join(confirmCtx.sentEvents, ","); got != "video,text" {
		t.Fatalf("expected video before lesson text, got event order %q", got)
	}
	session, _ = repo.GetActiveSession(ctx, telegramID)
	if session.Status != db.SessionStatusKDIRLesson || session.WeakTopics != "интеграции,бд" {
		t.Fatalf("expected KDIR lesson with deterministic topics, got %+v", session)
	}

	readyCtx := newCtx(telegramID, "")
	readyCtx.data = "ready"
	if err := h.handleReadyCallback(readyCtx); err != nil {
		t.Fatalf("ready callback: %v", err)
	}
	if len(readyCtx.sent) != 1 || !strings.Contains(readyCtx.sent[0], "q") || !strings.Contains(readyCtx.sent[0], "КДИР") {
		t.Fatalf("expected first question after ready, got %v", readyCtx.sent)
	}
	if len(readyCtx.sentOptions) != 1 || len(readyCtx.sentOptions[0]) != 1 || readyCtx.sentOptions[0][0] != tele.ModeHTML {
		t.Fatalf("expected primary question to use Telegram HTML mode, got %+v", readyCtx.sentOptions)
	}
	session, _ = repo.GetActiveSession(ctx, telegramID)
	if session.Status != db.SessionStatusQuestionCycle {
		t.Fatalf("expected QUESTION_CYCLE after ready, got %s", session.Status)
	}
	if session.CurrentQuestionID == nil || *session.CurrentQuestionID != 1 {
		t.Fatalf("expected current_question_id=1, got %v", session.CurrentQuestionID)
	}
	if session.CycleCount != 0 {
		t.Fatalf("expected cycle_count still 0 before any answer is graded, got %d", session.CycleCount)
	}
	if lm.lastPickTopic != "интеграции" {
		t.Fatalf("expected PickQuestion to use first weak topic, got %q", lm.lastPickTopic)
	}
	if lm.pickCalls != 1 {
		t.Fatalf("expected 1 PickQuestion call so far, got %d", lm.pickCalls)
	}

	// Primary answer: feedback is followed by the bank follow-up, while the
	// cycle count remains unchanged until the second answer is evaluated.
	lm.replies = []string{"▸ ОБРАТНАЯ СВЯЗЬ\nПервичный разбор"}
	answerCtx := newCtx(telegramID, "мой ответ на вопрос 1")
	if err := h.handleMessage(answerCtx); err != nil {
		t.Fatalf("handleMessage primary answer 1: %v", err)
	}
	if len(answerCtx.sent) != 2 {
		t.Fatalf("expected feedback and follow-up, got %v", answerCtx.sent)
	}
	if !strings.Contains(answerCtx.sent[0], "ОБРАТНАЯ СВЯЗЬ") {
		t.Fatalf("expected the evaluation frame first, got %q", answerCtx.sent[0])
	}
	if !strings.Contains(answerCtx.sent[1], "<b>Follow-up q</b>") || !strings.Contains(answerCtx.sent[1], "уточняющий вопрос 1") {
		t.Fatalf("expected the follow-up second, got %q", answerCtx.sent[1])
	}
	if len(answerCtx.sentOptions) != 2 || len(answerCtx.sentOptions[1]) != 1 || answerCtx.sentOptions[1][0] != tele.ModeHTML {
		t.Fatalf("expected the follow-up in Telegram HTML mode, got %+v", answerCtx.sentOptions)
	}
	if lm.lastEvalQuestion == nil || lm.lastEvalQuestion.ID != 1 || lm.lastEvalQuestion.AnswerSenior != "senior answer" {
		t.Fatalf("expected Evaluate to receive the question fetched by id with its reference answers, got %+v", lm.lastEvalQuestion)
	}
	session, _ = repo.GetActiveSession(ctx, telegramID)
	if session.Status != db.SessionStatusQuestionCycle || session.CycleCount != 0 {
		t.Fatalf("expected QUESTION_CYCLE cycle_count=0 before follow-up, got status=%s count=%d", session.Status, session.CycleCount)
	}
	attempt, err := repo.GetActiveQuestionAttempt(ctx, session.ID)
	if err != nil || attempt.Status != db.QuestionAttemptWaitingFollowup {
		t.Fatalf("expected attempt waiting for follow-up, got attempt=%+v err=%v", attempt, err)
	}
	if lm.evalCalls != 1 || lm.pickCalls != 1 {
		t.Fatalf("expected 1 primary evaluation and still 1 picked question, got eval=%d pick=%d", lm.evalCalls, lm.pickCalls)
	}

	// The first follow-up gets mini-feedback and the second bank follow-up.
	lm.replies = []string{"▸ ОБРАТНАЯ СВЯЗЬ\nПервое уточнение разобрано"}
	followupCtx := newCtx(telegramID, "мой ответ на уточнение 1")
	if err := h.handleMessage(followupCtx); err != nil {
		t.Fatalf("handleMessage follow-up answer 1: %v", err)
	}
	if len(followupCtx.sent) != 2 || !strings.Contains(followupCtx.sent[1], "<b>Follow-up q2</b>") || !strings.Contains(followupCtx.sent[1], "уточняющий вопрос 2") {
		t.Fatalf("expected mini-feedback and second follow-up, got %v", followupCtx.sent)
	}
	if len(followupCtx.sentOptions) != 2 || len(followupCtx.sentOptions[1]) != 1 || followupCtx.sentOptions[1][0] != tele.ModeHTML {
		t.Fatalf("expected the second follow-up in Telegram HTML mode, got %+v", followupCtx.sentOptions)
	}
	session, _ = repo.GetActiveSession(ctx, telegramID)
	if session.CycleCount != 0 {
		t.Fatalf("block must not count before third answer, got %d", session.CycleCount)
	}
	attempt, _ = repo.GetActiveQuestionAttempt(ctx, session.ID)
	if attempt.Status != db.QuestionAttemptWaitingFollowup2 {
		t.Fatalf("expected second follow-up state, got %s", attempt.Status)
	}

	// The third answer receives its own mini-feedback, then the large KDIR
	// review. Only now does the completed-block count increase.
	lm.replies = []string{"▸ ОБРАТНАЯ СВЯЗЬ\nПробел остался\n\n▸ БОЛЬШАЯ ОБРАТНАЯ СВЯЗЬ\nКДИР-разбор\nWEAK_ZONE_STATUS: confirmed"}
	secondFollowupCtx := newCtx(telegramID, "мой ответ на второе уточнение 1")
	if err := h.handleMessage(secondFollowupCtx); err != nil {
		t.Fatalf("handleMessage second follow-up answer 1: %v", err)
	}
	if len(secondFollowupCtx.sent) != 3 || strings.Contains(strings.Join(secondFollowupCtx.sent, "\n"), "WEAK_ZONE_STATUS") {
		t.Fatalf("expected third mini-feedback, big feedback and vector prompt, got %v", secondFollowupCtx.sent)
	}
	session, _ = repo.GetActiveSession(ctx, telegramID)
	if session.CycleCount != 1 {
		t.Fatalf("expected one completed block, got %d", session.CycleCount)
	}
	attempt, _ = repo.GetActiveQuestionAttempt(ctx, session.ID)
	if attempt.Status != db.QuestionAttemptWaitingVector {
		t.Fatalf("expected vector choice state, got %s", attempt.Status)
	}
	confirmedDB := false
	for _, zone := range repo.weakZones[telegramID] {
		if zone.ZoneText == "бд" && zone.Status == db.WeakZoneStatusConfirmed {
			confirmedDB = true
		}
	}
	if !confirmedDB {
		t.Fatalf("expected question topic to become confirmed weak zone, got %+v", repo.weakZones[telegramID])
	}

	// A vector choice completes the attempt and immediately asks question 2.
	vectorCtx := newCtx(telegramID, "")
	vectorCtx.data = "random"
	if err := h.handleVectorCallback(vectorCtx); err != nil {
		t.Fatalf("handleVectorCallback: %v", err)
	}
	if len(vectorCtx.sent) != 1 || !strings.Contains(vectorCtx.sent[0], "q") {
		t.Fatalf("expected question 2 after vector choice, got %v", vectorCtx.sent)
	}

	lm.replies = []string{"▸ ОБРАТНАЯ СВЯЗЬ\nПервичный разбор 2"}
	if err := h.handleMessage(newCtx(telegramID, "мой ответ на вопрос 2")); err != nil {
		t.Fatalf("handleMessage primary answer 2: %v", err)
	}

	lm.replies = []string{"▸ ОБРАТНАЯ СВЯЗЬ\nРазбор первого уточнения 2"}
	if err := h.handleMessage(newCtx(telegramID, "мой ответ на уточнение 2")); err != nil {
		t.Fatalf("handleMessage first follow-up in block 2: %v", err)
	}

	// At the configured limit the bot asks rather than ending automatically.
	lm.replies = []string{"▸ ОБРАТНАЯ СВЯЗЬ\nПробел закрыт\n\n▸ БОЛЬШАЯ ОБРАТНАЯ СВЯЗЬ\nКДИР-разбор 2\nWEAK_ZONE_STATUS: closed"}
	finalCtx := newCtx(telegramID, "мой ответ на второе уточнение 2")
	if err := h.handleMessage(finalCtx); err != nil {
		t.Fatalf("handleMessage second follow-up in block 2: %v", err)
	}
	if len(finalCtx.sent) != 3 || finalCtx.sent[2] != "Хочешь продолжить тренировку?" {
		t.Fatalf("expected feedback and soft-limit choice, got %v", finalCtx.sent)
	}

	lm.replies = []string{"Итоговый отчет"}
	finishCtx := newCtx(telegramID, "")
	finishCtx.data = "finish"
	if err := h.handleVectorCallback(finishCtx); err != nil {
		t.Fatalf("finish callback: %v", err)
	}
	if len(finishCtx.sent) != 1 || !strings.Contains(finishCtx.sent[0], "Итоговый отчет") ||
		!strings.Contains(finishCtx.sent[0], "/start SA2026-TEST") {
		t.Fatalf("expected final summary, got %v", finishCtx.sent)
	}
	if len(finishCtx.sentOptions) != 1 || len(finishCtx.sentOptions[0]) != 1 || finishCtx.sentOptions[0][0] != summaryMenu {
		t.Fatalf("expected return-to-questions button under summary, got %+v", finishCtx.sentOptions)
	}

	activeAfterSummary, err := repo.GetActiveSession(ctx, telegramID)
	if err != nil || activeAfterSummary.Status != db.SessionStatusSummary {
		t.Fatalf("expected active summary session, got session=%+v err=%v", activeAfterSummary, err)
	}
	if len(repo.summaries) != 1 || repo.summaries[0].SummaryText != "Итоговый отчет" {
		t.Fatalf("expected the summary to be saved, got %v", repo.summaries)
	}
	usage, err := repo.GetTokenUsageByAccessCode(ctx, "SA2026-TEST")
	if err != nil || usage.Calls != 7 || usage.PromptTokens != 70 || usage.CompletionTokens != 35 || usage.TotalTokens != 105 || usage.CachedTokens != 21 {
		t.Fatalf("expected every LLM call to be attributed to the access code, usage=%+v err=%v", usage, err)
	}

	returnCtx := newCtx(telegramID, "")
	returnCtx.data = "return"
	if err := h.handleReturnToQuestionsCallback(returnCtx); err != nil {
		t.Fatalf("return to questions callback: %v", err)
	}
	if len(returnCtx.sent) != 1 || returnCtx.sent[0] != "Куда двигаемся в следующем блоке?" {
		t.Fatalf("expected topic selection after summary, got %v", returnCtx.sent)
	}
	activeAfterReturn, err := repo.GetActiveSession(ctx, telegramID)
	if err != nil || activeAfterReturn.Status != db.SessionStatusQuestionCycle {
		t.Fatalf("expected resumed question cycle, got session=%+v err=%v", activeAfterReturn, err)
	}
	if len(repo.summaries) != 0 {
		t.Fatalf("stale summary must be deleted before continuing, got %v", repo.summaries)
	}
	if lm.evalCalls != 2 || lm.followupEvalCalls != 2 || lm.blockEvalCalls != 2 {
		t.Fatalf("expected two evaluations at every answer position, got primary=%d followup=%d block=%d", lm.evalCalls, lm.followupEvalCalls, lm.blockEvalCalls)
	}
	if lm.pickCalls != 2 {
		t.Fatalf("expected exactly 2 picked primary questions, got %d", lm.pickCalls)
	}
}

func TestTargetGradeMenuContainsOnlyMiddleAndSenior(t *testing.T) {
	if len(targetGradeMenu.InlineKeyboard) != 1 || len(targetGradeMenu.InlineKeyboard[0]) != 2 {
		t.Fatalf("unexpected target grade keyboard: %+v", targetGradeMenu.InlineKeyboard)
	}
	got := []string{targetGradeMenu.InlineKeyboard[0][0].Text, targetGradeMenu.InlineKeyboard[0][1].Text}
	if strings.Join(got, ",") != "Мидл,Сеньор" {
		t.Fatalf("target grades = %v, want [Мидл Сеньор]", got)
	}
	if len(currentGradeMenu.InlineKeyboard) != 1 || len(currentGradeMenu.InlineKeyboard[0]) != 3 {
		t.Fatalf("current grade keyboard must retain junior: %+v", currentGradeMenu.InlineKeyboard)
	}
}

func TestLegacyJuniorTargetCanBeReplacedWithoutLosingSession(t *testing.T) {
	repo := newFakeRepo()
	student := &db.Student{TelegramID: 77, Name: "Тест", AccessCode: "SA2026-LEGACY"}
	session := &db.Session{
		ID: 1, StudentID: student.TelegramID, Status: db.SessionStatusQuestionCycle,
		CurrentGrade: "джун", Grade: "джун", StrongZones: "требования", WeakZonesInput: "бд",
	}
	question := &db.QuestionBank{ID: 10, QuestionText: "Новый вопрос", Grade: "мидл-сеньор", Topic: "бд"}
	repo.students[student.TelegramID] = student
	repo.sessions[session.ID] = session
	repo.questions[question.ID] = question
	lm := &fakeLLM{question: question}
	h := New(repo, lm, slog.New(slog.NewTextHandler(io.Discard, nil)), 8, nil)

	requestCtx := newCtx(student.TelegramID, "")
	if err := h.askNextQuestion(context.Background(), requestCtx, student, session); err != nil {
		t.Fatalf("askNextQuestion legacy target: %v", err)
	}
	if len(requestCtx.sent) != 1 || !strings.Contains(requestCtx.sent[0], "мидл или сеньор") || lm.pickCalls != 0 {
		t.Fatalf("expected replacement target prompt before picking, got sent=%v picks=%d", requestCtx.sent, lm.pickCalls)
	}

	chooseCtx := newCtx(student.TelegramID, "")
	chooseCtx.data = "мидл"
	if err := h.handleGradeCallback(chooseCtx); err != nil {
		t.Fatalf("replace legacy target: %v", err)
	}
	updated, _ := repo.GetActiveSession(context.Background(), student.TelegramID)
	if updated.Grade != "мидл" || updated.CurrentGrade != "джун" || updated.StrongZones != "требования" || updated.WeakZonesInput != "бд" {
		t.Fatalf("legacy profile or progress was lost: %+v", updated)
	}
	if lm.pickCalls != 1 || lm.lastPickGrade != "мидл" || len(chooseCtx.sent) != 1 || !strings.Contains(chooseCtx.sent[0], "Новый вопрос") {
		t.Fatalf("expected middle question after replacement, sent=%v grade=%q picks=%d", chooseCtx.sent, lm.lastPickGrade, lm.pickCalls)
	}
}

func TestSendKDIRVideoUsesConfiguredTelegramFileID(t *testing.T) {
	h := New(newFakeRepo(), &fakeLLM{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 8, nil, MediaConfig{
		KDIRVideoFileID: "telegram-video-id",
	})
	ctx := newCtx(42, "")

	if err := h.sendKDIRVideo(ctx); err != nil {
		t.Fatalf("sendKDIRVideo: %v", err)
	}
	if len(ctx.sentVideos) != 1 || ctx.sentVideos[0].FileID != "telegram-video-id" || ctx.sentVideos[0].FileLocal != "" {
		t.Fatalf("expected Telegram file_id without local upload, got %+v", ctx.sentVideos)
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

func TestStartWithOriginalCodeImmediatelyStartsOver(t *testing.T) {
	repo := newFakeRepo("SA2026-TEST")
	h := New(repo, &fakeLLM{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 8, nil)
	const telegramID = 8

	if err := h.handleStart(newCtx(telegramID, "/start SA2026-TEST", "SA2026-TEST")); err != nil {
		t.Fatalf("initial start: %v", err)
	}
	oldSession, err := repo.GetActiveSession(context.Background(), telegramID)
	if err != nil {
		t.Fatalf("get initial session: %v", err)
	}

	restart := newCtx(telegramID, "/start SA2026-TEST", "SA2026-TEST")
	if err := h.handleStart(restart); err != nil {
		t.Fatalf("restart by original code: %v", err)
	}
	if len(restart.sent) != 2 || restart.sent[0] != instructionMessage || restart.sent[1] != currentGradeQuestion {
		t.Fatalf("expected immediate fresh qualification, got %v", restart.sent)
	}
	oldAfter, err := repo.sessionByID(oldSession.ID)
	if err != nil || oldAfter.Status != db.SessionStatusAbandoned || oldAfter.EndedAt == nil {
		t.Fatalf("old session must remain as abandoned history, session=%+v err=%v", oldAfter, err)
	}
	fresh, err := repo.GetActiveSession(context.Background(), telegramID)
	if err != nil || fresh.ID == oldSession.ID || fresh.Status != db.SessionStatusQualification {
		t.Fatalf("expected fresh qualification session, session=%+v err=%v", fresh, err)
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
	dirty.Direction = "старое направление"
	dirty.Experience = "старый опыт"
	dirty.InterviewTarget = "старая вакансия"
	dirty.StrongZones = "старые сильные зоны"
	dirty.WeakZonesInput = "старые слабые зоны"
	dirty.QualificationStep = db.QualificationStepWeakZones

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
	if fresh.Direction != "" || fresh.Experience != "" || fresh.InterviewTarget != "" {
		t.Errorf("new qualification fields not reset: direction=%q experience=%q target=%q",
			fresh.Direction, fresh.Experience, fresh.InterviewTarget)
	}
	if fresh.StrongZones != "" || fresh.WeakZonesInput != "" {
		t.Errorf("new stakeholder qualification fields not reset: strong=%q weak=%q",
			fresh.StrongZones, fresh.WeakZonesInput)
	}
	if fresh.QualificationStep != db.QualificationStepCurrentGrade {
		t.Errorf("qualification_step not reset: got %d", fresh.QualificationStep)
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

func TestSplitTelegramText(t *testing.T) {
	message := strings.Repeat("абвгд", 1000) + "\n" + strings.Repeat("финал ", 300)
	chunks := splitTelegramText(message, 3900)
	if len(chunks) < 2 {
		t.Fatalf("expected a long message to be split, got %d chunk", len(chunks))
	}
	for i, chunk := range chunks {
		if len([]rune(chunk)) > 3900 {
			t.Fatalf("chunk %d is too long: %d runes", i, len([]rune(chunk)))
		}
	}
	if got := splitTelegramText("  коротко  ", 3900); len(got) != 1 || got[0] != "коротко" {
		t.Fatalf("unexpected short split: %v", got)
	}
}

// seedStudentWithReport plants a student, and optionally a completed
// session and weak zones, directly into the fake so /report tests don't
// have to run a full registration + FSM flow per student.
func seedStudentWithReport(repo *fakeRepo, telegramID int64, name string, completedSessions int, lastSessionAt time.Time, zones ...db.WeakZone) {
	code := fmt.Sprintf("SA2026-R%03d", telegramID)
	repo.codes[code] = true
	repo.students[telegramID] = &db.Student{TelegramID: telegramID, Name: name, AccessCode: code, CreatedAt: lastSessionAt}
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

func TestAdminCodeManagementAndTokenUsage(t *testing.T) {
	repo := newFakeRepo()
	const adminID = int64(999)
	h := New(repo, &fakeLLM{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 8, []int64{adminID})

	nonAdmin := newCtx(123, "/code 2", "2")
	if err := h.handleCreateCodes(nonAdmin); err != nil || len(nonAdmin.sent) != 0 {
		t.Fatalf("non-admin command must be silent, sent=%v err=%v", nonAdmin.sent, err)
	}

	help := newCtx(adminID, "/admin")
	if err := h.handleAdminHelp(help); err != nil || len(help.sent) != 1 || !strings.Contains(help.sent[0], "/delete_code") {
		t.Fatalf("unexpected admin help: sent=%v err=%v", help.sent, err)
	}

	create := newCtx(adminID, "/code 2", "2")
	if err := h.handleCreateCodes(create); err != nil {
		t.Fatalf("create codes: %v", err)
	}
	if len(create.sent) != 1 || !strings.Contains(create.sent[0], "SA2026-A001") || !strings.Contains(create.sent[0], "SA2026-A002") {
		t.Fatalf("unexpected generated codes: %v", create.sent)
	}

	unusedTokens := newCtx(adminID, "/tokens SA2026-A001", "SA2026-A001")
	if err := h.handleTokenUsage(unusedTokens); err != nil || len(unusedTokens.sent) != 1 || !strings.Contains(unusedTokens.sent[0], "не использован") {
		t.Fatalf("unexpected unused-code usage: sent=%v err=%v", unusedTokens.sent, err)
	}

	student, err := repo.CreateStudentIfAccessCodeValid(context.Background(), 42, "Иван", "SA2026-A001")
	if err != nil {
		t.Fatalf("register generated code: %v", err)
	}
	session, err := repo.StartSession(context.Background(), student.TelegramID)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := repo.SaveLLMUsage(context.Background(), student.TelegramID, session.ID, llmOperationPrimaryFeedback, 100, 25, 125, 80); err != nil {
		t.Fatalf("save usage: %v", err)
	}

	usedTokens := newCtx(adminID, "/tokens SA2026-A001", "SA2026-A001")
	if err := h.handleTokenUsage(usedTokens); err != nil {
		t.Fatalf("get used-code tokens: %v", err)
	}
	for _, want := range []string{"Иван", "Вызовов модели: 1", "Входные токены: 100", "Всего токенов: 125", "Из них кэшировано: 80"} {
		if len(usedTokens.sent) != 1 || !strings.Contains(usedTokens.sent[0], want) {
			t.Fatalf("token report missing %q: %v", want, usedTokens.sent)
		}
	}

	listCodes := newCtx(adminID, "/codes")
	if err := h.handleListCodes(listCodes); err != nil {
		t.Fatalf("list codes: %v", err)
	}
	for _, want := range []string{"SA2026-A001 — занят", "Иван, Telegram ID: 42", "Токены: 125", "SA2026-A002 — свободен"} {
		if len(listCodes.sent) != 1 || !strings.Contains(listCodes.sent[0], want) {
			t.Fatalf("code list missing %q: %v", want, listCodes.sent)
		}
	}

	deleteUsed := newCtx(adminID, "/delete_code SA2026-A001", "SA2026-A001")
	if err := h.handleDeleteCode(deleteUsed); err != nil || len(deleteUsed.sent) != 1 || !strings.Contains(deleteUsed.sent[0], "Удаление запрещено") {
		t.Fatalf("used code must be protected: sent=%v err=%v", deleteUsed.sent, err)
	}
	deleteUnused := newCtx(adminID, "/delete_code SA2026-A002", "SA2026-A002")
	if err := h.handleDeleteCode(deleteUnused); err != nil || len(deleteUnused.sent) != 1 || !strings.Contains(deleteUnused.sent[0], "удалён") {
		t.Fatalf("unused code deletion failed: sent=%v err=%v", deleteUnused.sent, err)
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
		"Код: SA2026-R001", "Токены: 0",
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
	wantHeader := []string{
		"telegram_id", "name", "access_code", "completed_sessions", "last_session_at",
		"llm_calls", "prompt_tokens", "completion_tokens", "total_tokens", "cached_tokens",
	}
	if strings.Join(records[0], ",") != strings.Join(wantHeader, ",") {
		t.Fatalf("unexpected csv header: %v", records[0])
	}
	if records[1][2] == "" {
		t.Fatalf("expected access code in CSV, got %v", records[1])
	}
}

func TestQualification_InvalidGradeDoesNotAdvanceOrCallLLM(t *testing.T) {
	repo := newFakeRepo("SA2026-TEST")
	lm := &fakeLLM{replies: []string{"this reply must stay unused"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(repo, lm, logger, 8, nil)

	const telegramID = 55
	ctx := context.Background()

	if err := h.handleStart(newCtx(telegramID, "/start SA2026-TEST", "SA2026-TEST")); err != nil {
		t.Fatalf("initial start: %v", err)
	}

	invalid := newCtx(telegramID, "архитектор")
	if err := h.handleMessage(invalid); err != nil {
		t.Fatalf("invalid grade: %v", err)
	}
	if len(invalid.sent) != 1 || !strings.Contains(invalid.sent[0], "джун, мидл или сеньор") {
		t.Fatalf("expected a grade choice prompt, got %v", invalid.sent)
	}

	session, err := repo.GetActiveSession(ctx, telegramID)
	if err != nil {
		t.Fatalf("GetActiveSession: %v", err)
	}
	if session.Status != db.SessionStatusQualification || session.QualificationStep != db.QualificationStepCurrentGrade {
		t.Fatalf("invalid grade must not advance qualification, got status=%s step=%d", session.Status, session.QualificationStep)
	}
	if session.CurrentGrade != "" {
		t.Fatalf("invalid grade must not be stored, got %q", session.CurrentGrade)
	}
	if len(lm.replies) != 1 {
		t.Fatalf("qualification must not consume LLM replies")
	}

	valid := newCtx(telegramID, "джун")
	if err := h.handleMessage(valid); err != nil {
		t.Fatalf("valid text grade: %v", err)
	}
	session, _ = repo.GetActiveSession(ctx, telegramID)
	if session.CurrentGrade != "джун" || session.QualificationStep != db.QualificationStepTargetGrade {
		t.Fatalf("valid grade should advance exactly once, got %+v", session)
	}
}

func TestFormatQualificationSummaryCleansBulletLists(t *testing.T) {
	session := &db.Session{
		CurrentGrade:   "джун",
		Grade:          "мидл",
		StrongZones:    "- Безопасность",
		WeakZonesInput: "- Интеграции\n- Базы данных\n- Архитектура",
	}

	got := formatQualificationSummary(session)
	for _, want := range []string{
		"Текущий грейд: Джун",
		"Целевой грейд: Мидл",
		"Сильные зоны: Безопасность",
		"Слабые зоны: Интеграции, Базы данных, Архитектура",
		"В первую очередь будем подтягивать: Интеграции, Базы данных, Архитектура.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary does not contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\n-") {
		t.Fatalf("summary still contains raw bullet newlines:\n%s", got)
	}
	if btnReady.Text != "Готов(а)" {
		t.Fatalf("ready button text = %q, want %q", btnReady.Text, "Готов(а)")
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

func TestQuestionCycle_RecoversLegacyCurrentQuestionWithoutAttempt(t *testing.T) {
	repo := newFakeRepo()
	questionID := int64(99)
	question := &db.QuestionBank{ID: questionID, QuestionText: "legacy q", Followup1: "legacy follow-up"}
	repo.questions[questionID] = question
	student := &db.Student{TelegramID: 88}
	session := &db.Session{
		ID: 1, StudentID: 88, Status: db.SessionStatusQuestionCycle,
		Grade: "мидл", CurrentQuestionID: &questionID,
	}
	repo.sessions[session.ID] = session
	lm := &fakeLLM{replies: []string{"▸ ОБРАТНАЯ СВЯЗЬ\nразбор"}}
	h := New(repo, lm, slog.New(slog.NewTextHandler(io.Discard, nil)), 8, nil)
	ctx := newCtx(88, "какой-то ответ")

	if err := h.handleQuestionCycle(context.Background(), ctx, student, session); err != nil {
		t.Fatalf("handleQuestionCycle: %v", err)
	}
	if len(ctx.sent) != 2 || !strings.Contains(ctx.sent[1], "<b>Legacy follow-up</b>") {
		t.Fatalf("expected recovered attempt feedback and follow-up, got %v", ctx.sent)
	}
	attempt, err := repo.GetActiveQuestionAttempt(context.Background(), session.ID)
	if err != nil || attempt.Status != db.QuestionAttemptWaitingFollowup {
		t.Fatalf("expected recovered attempt waiting follow-up, got attempt=%+v err=%v", attempt, err)
	}
}

func TestQuestionCycle_RedeliversPrimaryFeedbackWithoutConsumingRetryText(t *testing.T) {
	repo := newFakeRepo()
	student := &db.Student{TelegramID: 88}
	session := &db.Session{ID: 1, StudentID: 88, Status: db.SessionStatusQuestionCycle, Grade: "мидл"}
	repo.sessions[session.ID] = session
	repo.attempts[1] = &db.QuestionAttempt{
		ID: 1, SessionID: session.ID, QuestionID: 9,
		PrimaryAnswer: "исходный ответ", PrimaryFeedback: "сохраненная обратная связь",
		FollowupQuestion: "сохраненное уточнение", Status: db.QuestionAttemptPrimaryFeedbackReady,
	}
	h := New(repo, &fakeLLM{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 8, nil)
	ctx := newCtx(student.TelegramID, "повтор исходного ответа")

	if err := h.handleQuestionCycle(context.Background(), ctx, student, session); err != nil {
		t.Fatalf("handleQuestionCycle: %v", err)
	}
	if got := ctx.sent; len(got) != 2 || got[0] != "сохраненная обратная связь" || got[1] != "сохраненное уточнение" {
		t.Fatalf("unexpected redelivery: %v", got)
	}
	attempt := repo.attempts[1]
	if attempt.PrimaryAnswer != "исходный ответ" || attempt.Status != db.QuestionAttemptWaitingFollowup {
		t.Fatalf("retry text was consumed or state did not advance: %+v", attempt)
	}
}

func TestQuestionCycle_RedeliversFollowupFeedbackWithoutConsumingRetryText(t *testing.T) {
	repo := newFakeRepo()
	student := &db.Student{TelegramID: 881}
	session := &db.Session{ID: 1, StudentID: student.TelegramID, Status: db.SessionStatusQuestionCycle, Grade: "мидл"}
	repo.sessions[session.ID] = session
	repo.attempts[1] = &db.QuestionAttempt{
		ID: 1, SessionID: session.ID, QuestionID: 9,
		FollowupAnswer: "сохраненный второй ответ", FollowupFeedback: "сохраненная обратная связь 2",
		Followup2Question: "сохраненное второе уточнение", Status: db.QuestionAttemptFollowupFeedbackReady,
	}
	h := New(repo, &fakeLLM{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 8, nil)
	ctx := newCtx(student.TelegramID, "повтор второго ответа")

	if err := h.handleQuestionCycle(context.Background(), ctx, student, session); err != nil {
		t.Fatalf("handleQuestionCycle: %v", err)
	}
	if got := ctx.sent; len(got) != 2 || got[0] != "сохраненная обратная связь 2" || got[1] != "сохраненное второе уточнение" {
		t.Fatalf("unexpected redelivery: %v", got)
	}
	attempt := repo.attempts[1]
	if attempt.FollowupAnswer != "сохраненный второй ответ" || attempt.Status != db.QuestionAttemptWaitingFollowup2 {
		t.Fatalf("retry text was consumed or state did not advance: %+v", attempt)
	}
}

func TestQuestionCycle_RedeliversFinalFeedbackBeforeVector(t *testing.T) {
	repo := newFakeRepo()
	student := &db.Student{TelegramID: 89}
	session := &db.Session{
		ID: 1, StudentID: 89, Status: db.SessionStatusQuestionCycle,
		Grade: "мидл", CycleCount: 1, WeakTopics: "бд,интеграции",
	}
	question := &db.QuestionBank{ID: 9, QuestionText: "q", Topic: "бд"}
	repo.sessions[session.ID] = session
	repo.questions[question.ID] = question
	repo.attempts[1] = &db.QuestionAttempt{
		ID: 1, SessionID: session.ID, QuestionID: question.ID,
		Followup2Feedback: "сохраненная мини-обратная связь",
		FinalFeedback:     "сохраненный полный разбор", Status: db.QuestionAttemptFinalFeedbackReady,
	}
	h := New(repo, &fakeLLM{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 8, nil)
	ctx := newCtx(student.TelegramID, "повтор после сетевой ошибки")

	if err := h.handleQuestionCycle(context.Background(), ctx, student, session); err != nil {
		t.Fatalf("handleQuestionCycle: %v", err)
	}
	if got := ctx.sent; len(got) != 3 || got[0] != "сохраненная мини-обратная связь" || got[1] != "сохраненный полный разбор" || got[2] != "Куда двигаемся в следующем блоке?" {
		t.Fatalf("unexpected final feedback redelivery: %v", got)
	}
	if repo.attempts[1].Status != db.QuestionAttemptWaitingVector {
		t.Fatalf("attempt status = %s, want WAITING_VECTOR", repo.attempts[1].Status)
	}
}

func TestSplitBlockFeedback(t *testing.T) {
	mini, full := splitBlockFeedback("▸ ОБРАТНАЯ СВЯЗЬ\nКоротко\n\n▸ БОЛЬШАЯ ОБРАТНАЯ СВЯЗЬ\nКДИР")
	if mini != "▸ ОБРАТНАЯ СВЯЗЬ\nКоротко" || full != "▸ БОЛЬШАЯ ОБРАТНАЯ СВЯЗЬ\nКДИР" {
		t.Fatalf("unexpected split: mini=%q full=%q", mini, full)
	}
}

func TestNormalizeFeedbackHeadingSpacing(t *testing.T) {
	input := "▸ ОБРАТНАЯ СВЯЗЬ текст сразу\n\n\n▸ БОЛЬШАЯ ОБРАТНАЯ СВЯЗЬ\nтекст большого разбора"
	got := normalizeFeedbackHeadingSpacing(input)
	want := "▸ ОБРАТНАЯ СВЯЗЬ\n\nтекст сразу\n\n\n▸ БОЛЬШАЯ ОБРАТНАЯ СВЯЗЬ\n\nтекст большого разбора"
	if got != want {
		t.Fatalf("unexpected normalized feedback:\nwant: %q\n got: %q", want, got)
	}
}

func TestFormatPrimaryQuestionBoldsOnlyQuestionAndEscapesHTML(t *testing.T) {
	question := &db.QuestionBank{
		QuestionContext: "Контекст про A & B",
		QuestionText:    "Что выбрать: cache < database?",
	}
	got := formatPrimaryQuestion(question, true)

	if !strings.Contains(got, "Контекст про A &amp; B") {
		t.Fatalf("context was not escaped: %q", got)
	}
	if !strings.Contains(got, "<b>Что выбрать: cache &lt; database?</b>") {
		t.Fatalf("question is not safely bolded: %q", got)
	}
	if strings.Contains(got, "<b>Отлично") || strings.Contains(got, "<b>Контекст") || strings.Contains(got, "КДИР.</b>") {
		t.Fatalf("text outside the question was bolded: %q", got)
	}
}

func TestFormatFollowupQuestionCapitalizesAndBoldsOnlyQuestion(t *testing.T) {
	got := formatFollowupQuestion("Контекст про A & B", "а если cache < database?", 1)

	if !strings.Contains(got, "Контекст про A &amp; B") {
		t.Fatalf("follow-up context was not escaped: %q", got)
	}
	if !strings.Contains(got, "<b>А если cache &lt; database?</b>") {
		t.Fatalf("follow-up question was not capitalized and safely bolded: %q", got)
	}
	if strings.Contains(got, "<b>Теперь") || strings.Contains(got, "<b>Контекст") || strings.Contains(got, "структуры.</b>") {
		t.Fatalf("text outside the follow-up question was bolded: %q", got)
	}
}

func TestDeliverFollowupQuestionsUsesTelegramHTMLMode(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	primaryRepo := newFakeRepo()
	primaryAttempt := &db.QuestionAttempt{
		ID: 1, Status: db.QuestionAttemptPrimaryFeedbackReady,
		PrimaryFeedback: "обратная связь", FollowupQuestion: "<b>Первый дожим</b>",
	}
	primaryRepo.attempts[primaryAttempt.ID] = primaryAttempt
	primaryHandler := New(primaryRepo, &fakeLLM{}, logger, 8, nil)
	primaryCtx := newCtx(1, "")
	if err := primaryHandler.deliverPrimaryFeedback(context.Background(), primaryCtx, primaryAttempt); err != nil {
		t.Fatalf("deliver primary feedback: %v", err)
	}
	if len(primaryCtx.sentOptions) != 2 || len(primaryCtx.sentOptions[1]) != 1 || primaryCtx.sentOptions[1][0] != tele.ModeHTML {
		t.Fatalf("first follow-up was not sent in HTML mode: %+v", primaryCtx.sentOptions)
	}

	followupRepo := newFakeRepo()
	followupAttempt := &db.QuestionAttempt{
		ID: 2, Status: db.QuestionAttemptFollowupFeedbackReady,
		FollowupFeedback: "обратная связь", Followup2Question: "<b>Второй дожим</b>",
	}
	followupRepo.attempts[followupAttempt.ID] = followupAttempt
	followupHandler := New(followupRepo, &fakeLLM{}, logger, 8, nil)
	followupCtx := newCtx(1, "")
	if err := followupHandler.deliverFollowupFeedback(context.Background(), followupCtx, followupAttempt); err != nil {
		t.Fatalf("deliver follow-up feedback: %v", err)
	}
	if len(followupCtx.sentOptions) != 2 || len(followupCtx.sentOptions[1]) != 1 || followupCtx.sentOptions[1][0] != tele.ModeHTML {
		t.Fatalf("second follow-up was not sent in HTML mode: %+v", followupCtx.sentOptions)
	}
}

func TestAskNextQuestion_ExhaustedBankBuildsSummary(t *testing.T) {
	repo := newFakeRepo()
	student := &db.Student{TelegramID: 90, Name: "Тест", AccessCode: "SA2026-EMPTY"}
	session := &db.Session{ID: 1, StudentID: student.TelegramID, Status: db.SessionStatusQuestionCycle, Grade: "мидл"}
	repo.students[student.TelegramID] = student
	repo.sessions[session.ID] = session
	lm := &fakeLLM{pickErr: db.ErrNoMatchingQuestion, replies: []string{"Итог без вопросов"}}
	h := New(repo, lm, slog.New(slog.NewTextHandler(io.Discard, nil)), 8, nil)
	ctx := newCtx(student.TelegramID, "")

	if err := h.askNextQuestion(context.Background(), ctx, student, session); err != nil {
		t.Fatalf("askNextQuestion: %v", err)
	}
	if got := ctx.sent; len(got) != 2 || !strings.Contains(got[0], "вопросы в банке закончились") ||
		!strings.Contains(got[1], "Итог без вопросов") || !strings.Contains(got[1], "/start SA2026-EMPTY") {
		t.Fatalf("unexpected exhaustion flow: %v", got)
	}
	if repo.sessions[session.ID].Status != db.SessionStatusSummary || repo.sessions[session.ID].EndedAt != nil {
		t.Fatalf("session must remain active at summary: %+v", repo.sessions[session.ID])
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
		{"/code command", "/code"},
		{"/codes command", "/codes"},
		{"/delete_code command", "/delete_code"},
		{"/tokens command", "/tokens"},
		{"restart callback", &btnRestart},
		{"continue callback", &btnContinue},
		{"current grade callback", &btnCurrentGradeJunior},
		{"target grade callback", &btnTargetGradeJunior},
		{"confirm profile callback", &btnConfirmProfile},
		{"edit profile callback", &btnEditProfile},
		{"KDIR ready callback", &btnReady},
		{"vector callback", &btnVectorDeepen},
		{"summary return callback", &btnReturnToQuestions},
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

			ctx := newCtx(telegramID, "какой-то текст", "1")

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

func TestValidateCandidateAnswer(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantAnswer  string
		wantMessage bool
	}{
		{name: "trims text", input: "  ответ  ", wantAnswer: "ответ"},
		{name: "rejects whitespace", input: " \n\t ", wantMessage: true},
		{name: "accepts max runes", input: strings.Repeat("я", maxCandidateAnswerRunes), wantAnswer: strings.Repeat("я", maxCandidateAnswerRunes)},
		{name: "rejects too many runes", input: strings.Repeat("я", maxCandidateAnswerRunes+1), wantMessage: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			answer, message := validateCandidateAnswer(tc.input)
			if answer != tc.wantAnswer {
				t.Fatalf("answer = %q, want %q", answer, tc.wantAnswer)
			}
			if (message != "") != tc.wantMessage {
				t.Fatalf("message = %q, wantMessage=%v", message, tc.wantMessage)
			}
		})
	}
}

func TestShutdownWaitsForInFlightHandlerAndRejectsNewOnes(t *testing.T) {
	h := New(newFakeRepo(), &fakeLLM{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 8, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})

	tracked := h.trackHandler(func(tele.Context) error {
		close(started)
		<-release
		close(finished)
		return nil
	})
	go func() { _ = tracked(newCtx(1, "ответ")) }()
	<-started

	shortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := h.Shutdown(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown while handler is running = %v, want deadline exceeded", err)
	}

	calledAfterShutdown := false
	rejected := h.trackHandler(func(tele.Context) error {
		calledAfterShutdown = true
		return nil
	})
	if err := rejected(newCtx(1, "ещё ответ")); err != nil {
		t.Fatalf("rejected handler returned error: %v", err)
	}
	if calledAfterShutdown {
		t.Fatal("handler scheduled after shutdown should not run")
	}

	close(release)
	<-finished
	if err := h.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after handler finished: %v", err)
	}
}
