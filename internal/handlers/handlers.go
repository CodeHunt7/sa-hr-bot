// Package handlers wires Telegram commands and messages to the
// interview session state machine.
package handlers

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	tele "gopkg.in/telebot.v3"

	"sa-hr-bot/internal/db"
	"sa-hr-bot/internal/llm"
)

// Per-phase turn caps for the deterministic parts of the FSM. Session
// status is always re-read from the database before each message is
// processed (see handleMessage), so these caps are the only "state" the
// process itself needs to track, and they live in sessions.cycle_count,
// reset to 0 on every phase transition (see db.Repository.AdvancePhase).
const (
	// fallbackGrade is used if QUESTION_CYCLE starts without a grade
	// captured during qualification. It degrades PickQuestion to a mid-level default instead
	// of failing the session outright.
	fallbackGrade = "мидл"

	// reportInlineThreshold is the student count at or under which
	// /report replies with a plain text message instead of a CSV file.
	reportInlineThreshold = 15

	// reportTextSafetyLimit is a conservative cutoff comfortably under
	// Telegram's 4096-character message limit; a report that would
	// still be at or under reportInlineThreshold students but happens
	// to run long (verbose weak-zone text, long names) falls back to
	// CSV too.
	reportTextSafetyLimit = 3500
)

const genericErrorMessage = "Что-то сломалось на моей стороне. Попробуй написать еще раз через минуту."

const (
	instructionMessage = "Сначала я задам четыре коротких вопроса, чтобы понять твой уровень и опыт. Затем покажу мини-аудит вероятных слабых зон. После этого начнем разбирать технические вопросы с обратной связью и уточнениями."
	gradeQuestion      = "На какой грейд ты претендуешь или сейчас себя ощущаешь?"
	directionQuestion  = "В каком направлении и индустрии у тебя основной опыт или куда хочешь перейти?"
	experienceQuestion = "В каких задачах у тебя есть реальный опыт, а в каких его почти нет? Например: требования, интеграции, БД, UML, документация, тест-кейсы."
	targetQuestion     = "Есть конкретная вакансия или дата собеседования, к которому готовишься? Если нет, так и напиши."
)

// qualificationMarkerRE matches the CURRENT_GRADE:/TARGET_GRADE:/
// REQUEST:/SELF_ASSESSMENT: marker lines the model appends during
// QUALIFICATION, per prompts/system_prompt.md's "Технические метки"
// section. They are stripped from what the student sees.
var qualificationMarkerRE = regexp.MustCompile(`(?im)^[ \t]*(CURRENT_GRADE|TARGET_GRADE|REQUEST|SELF_ASSESSMENT)[ \t]*:[ \t]*(.+?)[ \t]*$`)

// weakTopicsMarkerRE matches the WEAK_TOPICS: marker line the model
// appends during AUDIT (mini-audit), per prompts/system_prompt.md's
// "Технические метки" section. Stripped from what the student sees.
var weakTopicsMarkerRE = regexp.MustCompile(`(?im)^[ \t]*WEAK_TOPICS[ \t]*:[ \t]*(.+?)[ \t]*$`)

// validTopics mirrors question_bank.topic's allowed values (see
// db.QuestionBank's doc comment). Used to discard any WEAK_TOPICS value
// the model might hallucinate outside that fixed set, since PickQuestion
// filters on exact equality.
var validTopics = map[string]bool{
	"интеграции":   true,
	"архитектура":  true,
	"бд":           true,
	"требования":   true,
	"безопасность": true,
	"подача":       true,
}

// placeholderMarkerValues are values seen in practice where the model
// writes a qualification marker with a stand-in for "I don't actually
// know this yet" instead of simply not writing the marker at all, as
// instructed (see prompts/system_prompt.md's "Технические метки").
// isPlaceholderMarkerValue treats these as if the marker were absent,
// so they never get saved as if they were a real answer.
var placeholderMarkerValues = map[string]bool{
	"неизвестно":  true,
	"не известно": true,
	"нет данных":  true,
	"не указано":  true,
	"unknown":     true,
	"n/a":         true,
	"нет":         true,
	"-":           true,
}

func isPlaceholderMarkerValue(v string) bool {
	return placeholderMarkerValues[strings.ToLower(strings.TrimSpace(v))]
}

// restartMenu offers the choice shown when a student sends /start while
// they already have an active session.
var (
	restartMenu = &tele.ReplyMarkup{}
	btnRestart  = restartMenu.Data("Начать заново", "restart_session")
	btnContinue = restartMenu.Data("Продолжить", "continue_session")

	gradeMenu      = &tele.ReplyMarkup{}
	btnGradeJunior = gradeMenu.Data("Джун", "qualification_grade", "джун")
	btnGradeMiddle = gradeMenu.Data("Мидл", "qualification_grade", "мидл")
	btnGradeSenior = gradeMenu.Data("Сеньор", "qualification_grade", "сеньор")
)

func init() {
	restartMenu.Inline(restartMenu.Row(btnRestart, btnContinue))
	gradeMenu.Inline(gradeMenu.Row(btnGradeJunior, btnGradeMiddle, btnGradeSenior))
}

// Repository is the subset of db.Repository this package depends on.
type Repository interface {
	CreateStudentIfAccessCodeValid(ctx context.Context, telegramID int64, name, code string) (*db.Student, error)
	GetStudentByTelegramID(ctx context.Context, telegramID int64) (*db.Student, error)
	GetActiveSession(ctx context.Context, studentID int64) (*db.Session, error)
	StartSession(ctx context.Context, studentID int64) (*db.Session, error)
	EndSession(ctx context.Context, sessionID int64, status string) error
	AdvancePhase(ctx context.Context, sessionID int64, newStatus string) error
	IncrementCycleCount(ctx context.Context, sessionID int64) (int, error)
	SetQualificationAnswer(ctx context.Context, sessionID int64, step int, answer string) error
	SetWeakTopics(ctx context.Context, sessionID int64, topics string) error
	SetCurrentQuestionID(ctx context.Context, sessionID int64, questionID *int64) error
	GetQuestionByID(ctx context.Context, id int64) (*db.QuestionBank, error)
	GetWeakZones(ctx context.Context, studentID int64) ([]db.WeakZone, error)
	SaveSummary(ctx context.Context, sessionID int64, summaryText string) (*db.SessionSummary, error)
	GetStudentReports(ctx context.Context) ([]db.StudentReport, error)
}

// LLM is the subset of *llm.Service this package depends on.
type LLM interface {
	Reply(ctx context.Context, userMessage string) (*llm.Reply, error)
	Evaluate(ctx context.Context, profile llm.StudentProfile, weakZones []db.WeakZone, studentAnswer string, question *db.QuestionBank) (*llm.Reply, error)
	PickQuestion(ctx context.Context, grade, topic string) (*db.QuestionBank, error)
}

// Handler groups the dependencies needed to run the interview session
// FSM (INSTRUCTION -> QUALIFICATION -> AUDIT -> QUESTION_CYCLE -> SUMMARY).
type Handler struct {
	repo              Repository
	llm               LLM
	logger            *slog.Logger
	sessionCycleLimit int
	adminIDs          map[int64]bool
}

// New creates a Handler with its dependencies. sessionCycleLimit is the
// same value substituted into the system prompt (SESSION_CYCLE_LIMIT);
// the handler needs its own copy to know when to move QUESTION_CYCLE to
// SUMMARY. adminIDs are the Telegram user IDs allowed to run /report.
func New(repo Repository, llmService LLM, logger *slog.Logger, sessionCycleLimit int, adminIDs []int64) *Handler {
	ids := make(map[int64]bool, len(adminIDs))
	for _, id := range adminIDs {
		ids[id] = true
	}
	return &Handler{
		repo:              repo,
		llm:               llmService,
		logger:            logger,
		sessionCycleLimit: sessionCycleLimit,
		adminIDs:          ids,
	}
}

// Register attaches all command/message/callback handlers to the bot.
//
// Every endpoint goes through h.handle, which applies recoverMiddleware
// directly as a per-call argument to bot.Handle (see its doc comment):
// that is the structural guarantee that a handler can never end up
// unwrapped, independent of call ordering. bot.Use(h.recoverMiddleware)
// is also set as a second, redundant layer: telebot merges it into
// every bot.Handle call's middleware automatically (see Bot.Handle),
// so it transparently covers any endpoint someone registers here later
// by calling bot.Handle directly instead of h.handle, which is exactly
// the mistake this whole setup exists to make harmless.
//
// Do not call bot.Handle directly anywhere else in this package or
// main.go: every endpoint must be added here, through h.handle, so this
// function stays the single place that can add a new one.
func (h *Handler) Register(bot *tele.Bot) {
	bot.Use(h.recoverMiddleware)

	h.handle(bot, "/start", h.handleStart)
	h.handle(bot, "/report", h.handleReport)
	h.handle(bot, &btnRestart, h.handleRestartCallback)
	h.handle(bot, &btnContinue, h.handleContinueCallback)
	h.handle(bot, &btnGradeJunior, h.handleGradeCallback)
	h.handle(bot, tele.OnText, h.handleMessage)
}

// handle registers fn for endpoint with recoverMiddleware applied
// directly, as a per-call middleware argument to bot.Handle, rather
// than relying solely on the global bot.Use chain. telebot bakes
// whatever middleware is passed here into the stored handler at this
// exact call, so there is no ordering to get wrong and no way for an
// endpoint registered through this helper to accidentally skip it.
func (h *Handler) handle(bot *tele.Bot, endpoint interface{}, fn tele.HandlerFunc) {
	bot.Handle(endpoint, fn, h.recoverMiddleware)
}

// recoverMiddleware is global middleware (see Register) that recovers
// from panics in any handler.
//
// telebot.v3 runs every handler in a bare goroutine (see its
// Bot.runHandler) with no recover of its own: an unrecovered panic
// there crashes the entire process instantly, with no chance to log
// through our slog logger or reply to the user. This is what actually
// happens on the "Что-то сломалось" reports with nothing in the logs:
// the process panics, and whatever restarts it (systemd, Docker, fly.io)
// does so silently from the operator's point of view.
func (h *Handler) recoverMiddleware(next tele.HandlerFunc) tele.HandlerFunc {
	return func(c tele.Context) error {
		defer func() {
			if r := recover(); r != nil {
				h.handlePanic(c, r)
			}
		}()
		return next(c)
	}
}

// handlePanic logs a recovered panic with full detail and tries to
// notify the student. It runs from inside recoverMiddleware's deferred
// recover, i.e. after the original panic is already caught - but
// whatever broke badly enough to panic the handler (a poisoned DB pool,
// a nil dependency, anything) can just as easily panic again inside the
// very code trying to log and report it: h.fsmStatusForLogging makes
// its own repository calls, and c.Send makes a real API call. This was
// not hypothetical: an early version of this function called
// fsmStatusForLogging unguarded and a broken repository took down the
// process a second time from inside the recovery path itself, silently
// undoing the whole point of recovering in the first place. The nested
// defer/recover here is what actually closes that hole.
func (h *Handler) handlePanic(c tele.Context, r any) {
	defer func() {
		if r2 := recover(); r2 != nil {
			h.logger.Error("panic while handling a panic (original panic below may be incomplete)",
				"secondary_panic", fmt.Sprint(r2),
				"original_panic", fmt.Sprint(r),
			)
		}
	}()

	telegramID := senderID(c)
	h.logger.Error("panic in telegram handler",
		"panic", fmt.Sprint(r),
		"stack", string(debug.Stack()),
		"telegram_id", telegramID,
		"text", c.Text(),
		"fsm_status", h.safeFsmStatusForLogging(context.Background(), telegramID),
	)
	_ = c.Send(genericErrorMessage)
}

// OnError is wired up as the bot's Settings.OnError callback (see
// cmd/bot/main.go). telebot defaults to a bare log.Println through the
// standard "log" package when this is left unset, entirely bypassing
// our structured slog logger, which is exactly how these errors were
// going unnoticed: they were being logged, just to a different, easy to
// miss place. It only fires for a non-nil error a handler actually
// returned (a panic never reaches here; see recoverMiddleware for that),
// which in this codebase means the handler's own attempt to send its
// reply to the student failed, since every other error path already
// logs before returning nil.
//
// Guarded by its own recover for the same reason handlePanic is: the
// fsm_status lookup below (or c.Send) failing badly enough to panic
// must not escape unlogged.
func (h *Handler) OnError(err error, c tele.Context) {
	defer func() {
		if r := recover(); r != nil {
			h.logger.Error("panic while handling a handler error",
				"secondary_panic", fmt.Sprint(r),
				"original_error", err,
			)
		}
	}()

	var telegramID int64
	var text string
	if c != nil {
		telegramID = senderID(c)
		text = c.Text()
	}

	h.logger.Error("telegram handler returned an error",
		"error", err,
		"telegram_id", telegramID,
		"text", text,
		"fsm_status", h.safeFsmStatusForLogging(context.Background(), telegramID),
	)

	if c != nil {
		_ = c.Send(genericErrorMessage)
	}
}

// safeFsmStatusForLogging wraps fsmStatusForLogging with its own
// recover, on top of fsmStatusForLogging's existing best-effort error
// handling: that function already turns a returned error into a status
// string, but a panic (as opposed to an error) from the repository
// would still escape it. See handlePanic's doc comment for why that
// distinction matters here specifically.
func (h *Handler) safeFsmStatusForLogging(ctx context.Context, telegramID int64) (status string) {
	defer func() {
		if r := recover(); r != nil {
			status = fmt.Sprintf("lookup_panicked: %v", r)
		}
	}()
	return h.fsmStatusForLogging(ctx, telegramID)
}

// fsmStatusForLogging best-effort resolves telegramID's current FSM
// status for error/panic logs, so an incident tells you not just what
// broke but where in the interview flow it happened. Lookup failures
// are folded into the returned string rather than propagated: a broken
// error log must never itself break logging. Call safeFsmStatusForLogging
// instead of this directly from any panic/error handling path: this
// function only guards against the repository returning an error, not
// against it panicking.
func (h *Handler) fsmStatusForLogging(ctx context.Context, telegramID int64) string {
	if telegramID == 0 {
		return "unknown (no sender)"
	}
	student, err := h.repo.GetStudentByTelegramID(ctx, telegramID)
	if errors.Is(err, db.ErrStudentNotFound) {
		return "not_registered"
	}
	if err != nil {
		return fmt.Sprintf("lookup_failed: %v", err)
	}
	session, err := h.repo.GetActiveSession(ctx, student.TelegramID)
	if errors.Is(err, db.ErrNoActiveSession) {
		return "no_active_session"
	}
	if err != nil {
		return fmt.Sprintf("lookup_failed: %v", err)
	}
	return session.Status
}

// senderID reads the Telegram user ID off c, or 0 if the update has no
// sender (e.g. some non-message update types).
func senderID(c tele.Context) int64 {
	if s := c.Sender(); s != nil {
		return s.ID
	}
	return 0
}

// handleStart handles "/start" and "/start CODE". An unregistered
// student must supply a valid code; a registered student with no active
// session gets a fresh one; a registered student with an active session
// is asked whether to restart or continue it.
func (h *Handler) handleStart(c tele.Context) error {
	ctx := context.Background()
	telegramID := senderID(c)

	student, err := h.repo.GetStudentByTelegramID(ctx, telegramID)
	switch {
	case errors.Is(err, db.ErrStudentNotFound):
		return h.registerAndStart(ctx, c, telegramID)
	case err != nil:
		h.logger.Error("get student", "error", err)
		return c.Send(genericErrorMessage)
	}

	_, err = h.repo.GetActiveSession(ctx, student.TelegramID)
	switch {
	case errors.Is(err, db.ErrNoActiveSession):
		return h.startFreshSession(ctx, c, student)
	case err != nil:
		h.logger.Error("get active session", "error", err)
		return c.Send(genericErrorMessage)
	default:
		return c.Send(
			"У тебя уже есть активная сессия. Начать заново или продолжить?",
			restartMenu,
		)
	}
}

// registerAndStart validates the access code from "/start CODE" and, on
// success, registers the student and starts their first session.
func (h *Handler) registerAndStart(ctx context.Context, c tele.Context, telegramID int64) error {
	args := c.Args()
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return c.Send("Чтобы начать, напиши /start и код доступа через пробел, например /start SA2026-ABCD")
	}
	code := strings.TrimSpace(args[0])

	student, err := h.repo.CreateStudentIfAccessCodeValid(ctx, telegramID, displayName(c.Sender()), code)
	switch {
	case errors.Is(err, db.ErrAccessCodeInvalid):
		return c.Send("Код не подходит. Проверь, что скопировал его без пробелов и опечаток, и попробуй еще раз.")
	case errors.Is(err, db.ErrStudentAlreadyRegistered):
		// Race: registered between our earlier lookup and this insert.
		student, err = h.repo.GetStudentByTelegramID(ctx, telegramID)
		if err != nil {
			h.logger.Error("get student after race", "error", err)
			return c.Send(genericErrorMessage)
		}
	case err != nil:
		h.logger.Error("create student", "error", err)
		return c.Send(genericErrorMessage)
	}

	return h.startFreshSession(ctx, c, student)
}

// startFreshSession creates a new session, sends the fixed phase 0
// instruction once, then starts deterministic qualification with a grade
// choice. The model is deliberately not involved in either action.
func (h *Handler) startFreshSession(ctx context.Context, c tele.Context, student *db.Student) error {
	session, err := h.repo.StartSession(ctx, student.TelegramID)
	if err != nil {
		h.logger.Error("start session", "error", err)
		return c.Send(genericErrorMessage)
	}

	if err := c.Send(instructionMessage); err != nil {
		return err
	}

	if err := h.repo.AdvancePhase(ctx, session.ID, db.SessionStatusQualification); err != nil {
		h.logger.Error("advance phase", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}
	return c.Send(gradeQuestion, gradeMenu)
}

// handleGradeCallback stores the explicit grade button choice and advances to
// the second qualification question. Text grade answers are also accepted by
// handleQualification for accessibility and recovery.
func (h *Handler) handleGradeCallback(c tele.Context) error {
	if err := c.Respond(); err != nil {
		h.logger.Warn("respond callback", "error", err)
	}

	ctx := context.Background()
	student, err := h.repo.GetStudentByTelegramID(ctx, senderID(c))
	if errors.Is(err, db.ErrStudentNotFound) {
		return c.Send("Похоже, ты еще не зарегистрирован. Напиши /start и код доступа, который тебе прислали.")
	}
	if err != nil {
		h.logger.Error("get student", "error", err)
		return c.Send(genericErrorMessage)
	}

	session, err := h.repo.GetActiveSession(ctx, student.TelegramID)
	if err != nil {
		h.logger.Error("get active session", "error", err)
		return c.Send(genericErrorMessage)
	}
	if session.Status != db.SessionStatusQualification || session.QualificationStep != db.QualificationStepGrade {
		return c.Send("Этот выбор уже сохранён. Продолжаем с текущего шага.")
	}

	grade, ok := normalizeGrade(c.Data())
	if !ok {
		return c.Send("Выбери грейд кнопкой: джун, мидл или сеньор.", gradeMenu)
	}
	if err := h.repo.SetQualificationAnswer(ctx, session.ID, db.QualificationStepGrade, grade); err != nil {
		h.logger.Error("set qualification grade", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}
	return c.Send(directionQuestion)
}

// handleRestartCallback ends the student's active session as abandoned
// and starts a fresh one.
func (h *Handler) handleRestartCallback(c tele.Context) error {
	if err := c.Respond(); err != nil {
		h.logger.Warn("respond callback", "error", err)
	}

	ctx := context.Background()
	student, err := h.repo.GetStudentByTelegramID(ctx, senderID(c))
	if errors.Is(err, db.ErrStudentNotFound) {
		return c.Send("Похоже, ты еще не зарегистрирован. Напиши /start и код доступа, который тебе прислали.")
	}
	if err != nil {
		h.logger.Error("get student", "error", err)
		return c.Send(genericErrorMessage)
	}

	session, err := h.repo.GetActiveSession(ctx, student.TelegramID)
	switch {
	case errors.Is(err, db.ErrNoActiveSession):
		// Already gone; nothing to abandon.
	case err != nil:
		h.logger.Error("get active session", "error", err)
		return c.Send(genericErrorMessage)
	default:
		if err := h.repo.EndSession(ctx, session.ID, db.SessionStatusAbandoned); err != nil {
			h.logger.Error("end session", "error", err)
			return c.Send(genericErrorMessage)
		}
	}

	return h.startFreshSession(ctx, c, student)
}

// handleContinueCallback confirms there is still an active session to
// continue, then acknowledges: the student's next regular message is
// routed by handleMessage using the session's current phase, re-read
// fresh from the database as always, so there is no session state to
// thread through here. The active-session check exists only to avoid
// confidently telling the student "let's continue" when, in some race
// (e.g. the session ended between showing this button and it being
// tapped), there is nothing left to continue.
func (h *Handler) handleContinueCallback(c tele.Context) error {
	if err := c.Respond(); err != nil {
		h.logger.Warn("respond callback", "error", err)
	}

	ctx := context.Background()
	student, err := h.repo.GetStudentByTelegramID(ctx, senderID(c))
	if errors.Is(err, db.ErrStudentNotFound) {
		return c.Send("Похоже, ты еще не зарегистрирован. Напиши /start и код доступа, который тебе прислали.")
	}
	if err != nil {
		h.logger.Error("get student", "error", err)
		return c.Send(genericErrorMessage)
	}

	_, err = h.repo.GetActiveSession(ctx, student.TelegramID)
	if errors.Is(err, db.ErrNoActiveSession) {
		return c.Send("Активной сессии уже нет. Напиши /start и код доступа, чтобы начать заново.")
	}
	if err != nil {
		h.logger.Error("get active session", "error", err)
		return c.Send(genericErrorMessage)
	}

	return c.Send("Хорошо, продолжаем с того места, где остановились. Пиши следующее сообщение.")
}

// handleMessage routes a plain text message to the phase-specific
// handler for the student's currently active session. The session's
// status is always re-read from the database first, per the FSM design:
// there is no in-memory session state.
func (h *Handler) handleMessage(c tele.Context) error {
	ctx := context.Background()
	telegramID := senderID(c)

	student, err := h.repo.GetStudentByTelegramID(ctx, telegramID)
	if errors.Is(err, db.ErrStudentNotFound) {
		return c.Send("Похоже, ты еще не зарегистрирован. Напиши /start и код доступа, который тебе прислали.")
	}
	if err != nil {
		h.logger.Error("get student", "error", err)
		return c.Send(genericErrorMessage)
	}

	session, err := h.repo.GetActiveSession(ctx, student.TelegramID)
	if errors.Is(err, db.ErrNoActiveSession) {
		return c.Send("Активной сессии нет. Напиши /start и код доступа, чтобы начать.")
	}
	if err != nil {
		h.logger.Error("get active session", "error", err)
		return c.Send(genericErrorMessage)
	}

	switch session.Status {
	case db.SessionStatusQualification:
		return h.handleQualification(ctx, c, student, session)
	case db.SessionStatusAudit:
		return h.handleAudit(ctx, c, student, session)
	case db.SessionStatusQuestionCycle:
		return h.handleQuestionCycle(ctx, c, student, session)
	case db.SessionStatusSummary:
		// Shouldn't normally be reached: handleQuestionCycle runs
		// SUMMARY itself as soon as it transitions into it. Handled
		// here too so a session stuck in this status is still able
		// to finish rather than going silent.
		return h.runSummary(ctx, c, student, session)
	default:
		h.logger.Error("unexpected session status", "status", session.Status, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}
}

// handleQualification runs the fixed four-question qualification flow. Each
// incoming answer is saved verbatim in the column for the current step; the
// model neither extracts fields nor chooses the next question.
func (h *Handler) handleQualification(ctx context.Context, c tele.Context, student *db.Student, session *db.Session) error {
	answer := strings.TrimSpace(c.Text())
	if answer == "" {
		return c.Send("Нужен текстовый ответ, чтобы я мог продолжить.")
	}

	switch session.QualificationStep {
	case db.QualificationStepGrade:
		grade, ok := normalizeGrade(answer)
		if !ok {
			return c.Send("Выбери грейд: джун, мидл или сеньор.", gradeMenu)
		}
		if err := h.repo.SetQualificationAnswer(ctx, session.ID, db.QualificationStepGrade, grade); err != nil {
			return h.qualificationSaveError(c, session.ID, err)
		}
		return c.Send(directionQuestion)

	case db.QualificationStepDirection:
		if err := h.repo.SetQualificationAnswer(ctx, session.ID, db.QualificationStepDirection, answer); err != nil {
			return h.qualificationSaveError(c, session.ID, err)
		}
		return c.Send(experienceQuestion)

	case db.QualificationStepExperience:
		if err := h.repo.SetQualificationAnswer(ctx, session.ID, db.QualificationStepExperience, answer); err != nil {
			return h.qualificationSaveError(c, session.ID, err)
		}
		return c.Send(targetQuestion)

	case db.QualificationStepInterviewTarget:
		if err := h.repo.SetQualificationAnswer(ctx, session.ID, db.QualificationStepInterviewTarget, answer); err != nil {
			return h.qualificationSaveError(c, session.ID, err)
		}
		session.InterviewTarget = answer
		session.QualificationStep = db.QualificationStepDone
		if err := h.repo.AdvancePhase(ctx, session.ID, db.SessionStatusAudit); err != nil {
			h.logger.Error("advance phase", "error", err, "session_id", session.ID)
			return c.Send(genericErrorMessage)
		}
		session.Status = db.SessionStatusAudit
		return h.runAuditAndStartQuestion(ctx, c, student, session)

	case db.QualificationStepDone:
		if err := h.repo.AdvancePhase(ctx, session.ID, db.SessionStatusAudit); err != nil {
			h.logger.Error("advance completed qualification to audit", "error", err, "session_id", session.ID)
			return c.Send(genericErrorMessage)
		}
		session.Status = db.SessionStatusAudit
		return h.runAuditAndStartQuestion(ctx, c, student, session)

	default:
		h.logger.Error("unexpected qualification step", "step", session.QualificationStep, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}
}

func (h *Handler) qualificationSaveError(c tele.Context, sessionID int64, err error) error {
	h.logger.Error("set qualification answer", "error", err, "session_id", sessionID)
	return c.Send(genericErrorMessage)
}

func normalizeGrade(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "джун", "джуниор", "junior":
		return "джун", true
	case "мидл", "middle":
		return "мидл", true
	case "сеньор", "сениор", "senior":
		return "сеньор", true
	default:
		return "", false
	}
}

// handleAudit runs phase 2: present the mini-audit frame. It is a single
// turn (auditTurnCap), then the FSM moves into the question cycle. The
// reply's WEAK_TOPICS marker is parsed and saved as a priority topic
// filter for QUESTION_CYCLE's PickQuestion calls.
func (h *Handler) handleAudit(ctx context.Context, c tele.Context, student *db.Student, session *db.Session) error {
	return h.runAuditAndStartQuestion(ctx, c, student, session)
}

func (h *Handler) runAuditAndStartQuestion(ctx context.Context, c tele.Context, student *db.Student, session *db.Session) error {
	weakZones, err := h.repo.GetWeakZones(ctx, student.TelegramID)
	if err != nil {
		h.logger.Error("get weak zones", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}

	profile := llm.QualificationProfile{
		Grade:           session.Grade,
		Direction:       session.Direction,
		Experience:      session.Experience,
		InterviewTarget: session.InterviewTarget,
	}

	reply, err := h.llm.Reply(ctx, llm.BuildAuditContext(profile, weakZones))
	if err != nil {
		h.logger.Error("llm reply", "error", err, "session_id", session.ID, "status", session.Status)
		return c.Send(genericErrorMessage)
	}

	topics, cleaned := extractWeakTopics(reply.Text)
	if len(topics) > 0 {
		session.WeakTopics = strings.Join(topics, ",")
		if err := h.repo.SetWeakTopics(ctx, session.ID, session.WeakTopics); err != nil {
			h.logger.Error("set weak topics", "error", err, "session_id", session.ID)
		}
	}

	if err := c.Send(cleaned); err != nil {
		return err
	}

	if err := h.repo.AdvancePhase(ctx, session.ID, db.SessionStatusQuestionCycle); err != nil {
		h.logger.Error("advance phase", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}
	session.Status = db.SessionStatusQuestionCycle
	session.CurrentQuestionID = nil
	return h.askNextQuestion(ctx, c, student, session)
}

// handleQuestionCycle runs phase 3. It is split into two steps that
// straddle two separate Telegram updates, tied together by
// session.CurrentQuestionID (there is no persisted message history to
// otherwise recover which question a given answer belongs to):
//
//   - CurrentQuestionID empty: pick a question and send it as-is. No
//     model call: there is nothing to evaluate yet.
//   - CurrentQuestionID set: the incoming message is the student's
//     answer to that exact question. Grade it, then either ask the next
//     question or move to SUMMARY.
func (h *Handler) handleQuestionCycle(ctx context.Context, c tele.Context, student *db.Student, session *db.Session) error {
	if session.CurrentQuestionID == nil {
		return h.askNextQuestion(ctx, c, student, session)
	}
	return h.evaluateAnswer(ctx, c, student, session)
}

// askNextQuestion picks a question from the bank matching the
// candidate's grade, preferring a topic tied to an active weak-zone
// hypothesis or the mini-audit's weak topics (see pickPriorityTopic),
// stores it as the session's current question, and sends its text to
// the student verbatim.
func (h *Handler) askNextQuestion(ctx context.Context, c tele.Context, student *db.Student, session *db.Session) error {
	grade := session.Grade
	if grade == "" {
		h.logger.Warn("question cycle started without a captured grade, using fallback",
			"session_id", session.ID, "fallback_grade", fallbackGrade)
		grade = fallbackGrade
	}

	topic, err := h.pickPriorityTopic(ctx, student, session)
	if err != nil {
		h.logger.Warn("pick priority topic", "error", err, "session_id", session.ID)
		// Not fatal: fall through with no topic filter.
	}

	question, err := h.llm.PickQuestion(ctx, grade, topic)
	if errors.Is(err, db.ErrNoMatchingQuestion) && topic != "" {
		// The priority topic had no matching question for this grade;
		// retry without it rather than dead-ending the cycle.
		question, err = h.llm.PickQuestion(ctx, grade, "")
	}
	if err != nil {
		h.logger.Error("pick question", "error", err, "grade", grade, "topic", topic, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}

	if err := h.repo.SetCurrentQuestionID(ctx, session.ID, &question.ID); err != nil {
		h.logger.Error("set current question id", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}

	return c.Send(question.QuestionText)
}

// pickPriorityTopic returns the topic PickQuestion should prioritize.
// A weak zone already tied to a concrete topic (its zone_text exactly
// matches a question_bank.topic value) wins over the broader weak_topics
// list from the mini-audit, since it means a specific hypothesis is
// actively being confirmed or refuted. Returns "" if neither is
// available, letting PickQuestion match any topic for the grade.
func (h *Handler) pickPriorityTopic(ctx context.Context, student *db.Student, session *db.Session) (string, error) {
	weakZones, err := h.repo.GetWeakZones(ctx, student.TelegramID)
	if err != nil {
		return "", err
	}
	for _, wz := range weakZones {
		if t := strings.ToLower(wz.ZoneText); validTopics[t] {
			return t, nil
		}
	}

	// weak_topics is normally only ever written by SetWeakTopics with
	// values already filtered through validTopics (see
	// extractWeakTopics), but re-validate here too rather than trust
	// that invariant blindly: a raw DB edit, an older row from before
	// that filtering existed, or a future write path that skips it
	// could otherwise hand PickQuestion a topic string it will never
	// match, which is a silent dead end rather than a crash but still
	// worth not doing.
	for _, t := range strings.Split(session.WeakTopics, ",") {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" && validTopics[t] {
			return t, nil
		}
	}

	return "", nil
}

// evaluateAnswer grades the incoming message against
// session.CurrentQuestionID's reference answers, then either asks the
// next question (chained into the same update, so the student doesn't
// need to send an extra message) or moves on to SUMMARY once the
// session's cycle limit is reached.
func (h *Handler) evaluateAnswer(ctx context.Context, c tele.Context, student *db.Student, session *db.Session) error {
	if session.CurrentQuestionID == nil {
		// handleQuestionCycle only calls evaluateAnswer when this is
		// non-nil; this guard exists so a future call site mistake (or
		// a session mutated between the check and here) degrades to a
		// logged, friendly error instead of a nil-pointer panic below.
		h.logger.Error("evaluateAnswer called with no current question", "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}

	question, err := h.repo.GetQuestionByID(ctx, *session.CurrentQuestionID)
	if err != nil {
		h.logger.Error("get question by id", "error", err, "session_id", session.ID, "question_id", *session.CurrentQuestionID)
		return c.Send(genericErrorMessage)
	}

	weakZones, err := h.repo.GetWeakZones(ctx, student.TelegramID)
	if err != nil {
		h.logger.Error("get weak zones", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}

	profile := llm.StudentProfile{Grade: session.Grade}

	reply, err := h.llm.Evaluate(ctx, profile, weakZones, c.Text(), question)
	if err != nil {
		h.logger.Error("llm evaluate", "error", err, "session_id", session.ID, "question_id", question.ID)
		return c.Send(genericErrorMessage)
	}

	if err := c.Send(reply.Text); err != nil {
		return err
	}

	if err := h.repo.SetCurrentQuestionID(ctx, session.ID, nil); err != nil {
		h.logger.Error("clear current question id", "error", err, "session_id", session.ID)
	}
	session.CurrentQuestionID = nil

	count, err := h.repo.IncrementCycleCount(ctx, session.ID)
	if err != nil {
		h.logger.Error("increment cycle count", "error", err, "session_id", session.ID)
		return nil // the evaluation already went out; don't fail the update over bookkeeping
	}

	if count < h.sessionCycleLimit {
		return h.askNextQuestion(ctx, c, student, session)
	}

	if err := h.repo.AdvancePhase(ctx, session.ID, db.SessionStatusSummary); err != nil {
		h.logger.Error("advance phase", "error", err, "session_id", session.ID)
		return nil
	}
	session.Status = db.SessionStatusSummary
	return h.runSummary(ctx, c, student, session)
}

// runSummary runs phase 4: ask the model for the final report, save it,
// and close out the session.
func (h *Handler) runSummary(ctx context.Context, c tele.Context, student *db.Student, session *db.Session) error {
	weakZones, err := h.repo.GetWeakZones(ctx, student.TelegramID)
	if err != nil {
		h.logger.Error("get weak zones", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}

	profile := llm.StudentProfile{Grade: session.Grade}

	reply, err := h.llm.Reply(ctx, llm.BuildUserContext(profile, weakZones, nil, nil))
	if err != nil {
		h.logger.Error("llm reply (summary)", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}

	if err := c.Send(reply.Text); err != nil {
		return err
	}

	if _, err := h.repo.SaveSummary(ctx, session.ID, reply.Text); err != nil {
		h.logger.Error("save summary", "error", err, "session_id", session.ID)
	}

	if err := h.repo.EndSession(ctx, session.ID, db.SessionStatusCompleted); err != nil {
		h.logger.Error("end session", "error", err, "session_id", session.ID)
	}

	return nil
}

// handleReport is an admin-only export of every registered student:
// telegram_id, name, completed session count, last session date, and
// current weak-zone map. If the caller's telegram_id is not in
// ADMIN_IDS, the command is silently ignored: no reply at all, so a
// non-admin poking at random commands can't even tell /report exists.
func (h *Handler) handleReport(c tele.Context) error {
	if !h.adminIDs[senderID(c)] {
		return nil
	}

	ctx := context.Background()
	reports, err := h.repo.GetStudentReports(ctx)
	if err != nil {
		h.logger.Error("get student reports", "error", err)
		return c.Send(genericErrorMessage)
	}

	if len(reports) == 0 {
		return c.Send("Пока нет ни одного зарегистрированного студента.")
	}

	text := formatReportText(reports)
	if len(reports) <= reportInlineThreshold && len(text) <= reportTextSafetyLimit {
		return c.Send(text)
	}

	path, err := writeReportCSV(reports)
	if err != nil {
		h.logger.Error("write report csv", "error", err)
		return c.Send(genericErrorMessage)
	}
	defer os.Remove(path)

	doc := &tele.Document{
		File:     tele.FromDisk(path),
		FileName: fmt.Sprintf("students_report_%s.csv", time.Now().Format("2006-01-02")),
	}
	return c.Send(doc)
}

// formatReportText renders reports as a plain-text block, one student
// per paragraph, for the small-enough-to-inline case.
func formatReportText(reports []db.StudentReport) string {
	var b strings.Builder
	for i, rep := range reports {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "telegram_id: %d\n", rep.TelegramID)
		fmt.Fprintf(&b, "Имя: %s\n", rep.Name)
		fmt.Fprintf(&b, "Завершенных сессий: %d\n", rep.CompletedSessions)
		fmt.Fprintf(&b, "Последняя сессия: %s\n", formatReportDate(rep.LastSessionAt))
		fmt.Fprintf(&b, "Слабые зоны: %s\n", formatWeakZones(rep.WeakZones))
	}
	return b.String()
}

// writeReportCSV writes reports to a new temporary CSV file and returns
// its path. The caller is responsible for removing it.
func writeReportCSV(reports []db.StudentReport) (string, error) {
	f, err := os.CreateTemp("", "students_report_*.csv")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if err := w.Write([]string{"telegram_id", "name", "completed_sessions", "last_session_at", "weak_zones"}); err != nil {
		return "", fmt.Errorf("write csv header: %w", err)
	}
	for _, rep := range reports {
		row := []string{
			strconv.FormatInt(rep.TelegramID, 10),
			rep.Name,
			strconv.Itoa(rep.CompletedSessions),
			formatReportDate(rep.LastSessionAt),
			formatWeakZones(rep.WeakZones),
		}
		if err := w.Write(row); err != nil {
			return "", fmt.Errorf("write csv row: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", fmt.Errorf("flush csv: %w", err)
	}

	return f.Name(), nil
}

// formatReportDate renders a nullable last-session timestamp for both
// the text and CSV report formats.
func formatReportDate(t *time.Time) string {
	if t == nil {
		return "нет"
	}
	return t.Format("2006-01-02 15:04")
}

// formatWeakZones renders a student's weak-zone map as "text (status)"
// pairs for both the text and CSV report formats.
func formatWeakZones(zones []db.WeakZone) string {
	if len(zones) == 0 {
		return "нет"
	}
	parts := make([]string, len(zones))
	for i, z := range zones {
		parts[i] = fmt.Sprintf("%s (%s)", z.ZoneText, z.Status)
	}
	return strings.Join(parts, "; ")
}

// extractQualificationMarkers pulls CURRENT_GRADE:/TARGET_GRADE:/
// REQUEST:/SELF_ASSESSMENT: marker lines (see prompts/system_prompt.md)
// out of text, returning their values (empty if absent) and the text
// with those lines removed.
func extractQualificationMarkers(text string) (currentGrade, targetGrade, request, selfAssessment, cleaned string) {
	cleaned = text
	for _, m := range qualificationMarkerRE.FindAllStringSubmatch(text, -1) {
		value := strings.TrimSpace(m[2])
		if isPlaceholderMarkerValue(value) {
			// Observed in practice: the model sometimes writes a marker
			// with a placeholder like "неизвестно" instead of simply
			// not writing it. Treating that as absent (rather than as
			// a real answer) stops "неизвестно" from being saved to
			// e.g. self_assessment as if the student had said it.
			value = ""
		}
		switch strings.ToUpper(m[1]) {
		case "CURRENT_GRADE":
			currentGrade = value
		case "TARGET_GRADE":
			targetGrade = value
		case "REQUEST":
			request = value
		case "SELF_ASSESSMENT":
			selfAssessment = value
		}
		cleaned = strings.Replace(cleaned, m[0], "", 1)
	}
	return currentGrade, targetGrade, request, selfAssessment, strings.TrimSpace(cleaned)
}

// extractWeakTopics pulls the WEAK_TOPICS: marker line (see
// prompts/system_prompt.md) out of text, returning its comma-separated
// values (filtered to validTopics, so a hallucinated topic outside the
// fixed set is silently dropped rather than breaking PickQuestion's
// exact-match filter) and the text with that line removed.
func extractWeakTopics(text string) (topics []string, cleaned string) {
	cleaned = text
	m := weakTopicsMarkerRE.FindStringSubmatch(text)
	if m == nil {
		return nil, cleaned
	}
	cleaned = strings.TrimSpace(strings.Replace(cleaned, m[0], "", 1))

	for _, raw := range strings.Split(m[1], ",") {
		if t := strings.ToLower(strings.TrimSpace(raw)); validTopics[t] {
			topics = append(topics, t)
		}
	}
	return topics, cleaned
}

// displayName builds a human-readable name for a Telegram user, used as
// the student's Name in the database.
func displayName(u *tele.User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name != "" {
		return name
	}
	if u.Username != "" {
		return u.Username
	}
	return "Студент"
}
