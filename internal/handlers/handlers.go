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
	"sync"
	"time"
	"unicode/utf8"

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

const telegramTextChunkLimit = 3900

const (
	handlerTimeout          = 90 * time.Second
	maxCandidateAnswerRunes = 8000
	studentLockStripeCount  = 64
)

const (
	welcomePhotoPath = "media/pic1.PNG"
	readyPhotoPath   = "media/pic2.PNG"

	instructionMessage = `Привет. Меня зовут Катя Желатинка, я собрала для тебя тренажёр для отработки технических собеседований системного аналитика.

Это не чат-бот общего назначения. Под капотом этого ИИ-агента зашито больше 150 вопросов, которые задают на собесах в 2026 года.

Формат простой: сначала 4 коротких вопроса про тебя, чтобы лучше понять твой запрос. Дальше цикл вопросов с разбором каждого ответа. Фидбек получаешь сразу, без ожидания.

Представь, что готовишься к экзамену на права для авто, все тоже самое, только готовимся к собеседованию.

Я буду задавать тебе основной вопрос и два дополнительных, а после буду давать обратную связь и разъяснение по твоим ответам.

Прежде чем начнём - 4 вопроса, чтобы подобрать нужный уровень сложности. Начинаем?`
	currentGradeQuestion = "Вопрос 1/4\nКакой у тебя текущий грейд?"
	targetGradeQuestion  = "Вопрос 2/4\nНа какой грейд собеседуешься?"
	strongZonesQuestion  = `Вопрос 3/4
Какие технические зоны ты знаешь лучше всего?

Напиши ответ текстом:
- Интеграции
- Базы данных
- Архитектура
- Требования
- Безопасность`
	weakZonesQuestion = `Вопрос 4/4
Какие технические зоны ты знаешь хуже всего?

Напиши ответ текстом:
- Интеграции
- Базы данных
- Архитектура
- Требования
- Безопасность`
	kdirLessonMessage = `Отлично, режде чем начнём отработку - один короткий урок.

Разбираю в нём формулу КДИР: Контекст, Действие, Инструмент, Результат.

Это формула и способ, который позволит отвечать на технические вопросы так, чтобы сразу был виден масштаб задачи, что сделал именно ты, каким инструментом и какой результат получил.

Без формулы ответ звучит как пересказ обязанностей. Сыро и иногда запутанно. А с ней, как конкретный кейс с цифрами, который HR может корректно оценить.

Смотри урок, переходи к тренажеру, а дальше на каждом вопросе тренажёра жду от тебя ответ именно по этой структуре.

Посмотри видео, потом жми «Готов(а)».`
	readyPrompt = "Ситуация понятна. Приступим?"
)

// weakTopicsMarkerRE matches the WEAK_TOPICS: marker line the model
// appends during AUDIT (mini-audit), per prompts/system_prompt.md's
// "Технические метки" section. Stripped from what the student sees.
var weakTopicsMarkerRE = regexp.MustCompile(`(?im)^[ \t]*WEAK_TOPICS[ \t]*:[ \t]*(.+?)[ \t]*$`)

// weakZoneStatusMarkerRE is emitted only by the final three-answer evaluation.
var weakZoneStatusMarkerRE = regexp.MustCompile(`(?im)^[ \t]*WEAK_ZONE_STATUS[ \t]*:[ \t]*(confirmed|closed)[ \t]*$`)

var bigFeedbackHeadingRE = regexp.MustCompile(`(?im)^[ \t]*▸[ \t]*БОЛЬШАЯ ОБРАТНАЯ СВЯЗЬ[ \t]*$`)

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

// restartMenu offers the choice shown when a student sends /start while
// they already have an active session.
var (
	restartMenu = &tele.ReplyMarkup{}
	btnRestart  = restartMenu.Data("Начать заново", "restart_session")
	btnContinue = restartMenu.Data("Продолжить", "continue_session")

	currentGradeMenu      = &tele.ReplyMarkup{}
	btnCurrentGradeJunior = currentGradeMenu.Data("Джун", "qualification_current_grade", "джун")
	btnCurrentGradeMiddle = currentGradeMenu.Data("Мидл", "qualification_current_grade", "мидл")
	btnCurrentGradeSenior = currentGradeMenu.Data("Сеньор", "qualification_current_grade", "сеньор")

	targetGradeMenu      = &tele.ReplyMarkup{}
	btnTargetGradeJunior = targetGradeMenu.Data("Джун", "qualification_target_grade", "джун")
	btnTargetGradeMiddle = targetGradeMenu.Data("Мидл", "qualification_target_grade", "мидл")
	btnTargetGradeSenior = targetGradeMenu.Data("Сеньор", "qualification_target_grade", "сеньор")

	confirmationMenu  = &tele.ReplyMarkup{}
	btnConfirmProfile = confirmationMenu.Data("Да, все верно.", "qualification_confirm", "confirm")
	btnEditProfile    = confirmationMenu.Data("Изменить ответы", "qualification_edit", "edit")

	readyMenu = &tele.ReplyMarkup{}
	btnReady  = readyMenu.Data("Готов(а), начинаем", "kdir_ready", "ready")

	vectorMenu         = &tele.ReplyMarkup{}
	btnVectorDeepen    = vectorMenu.Data("Углубиться в тему", "question_vector", "deepen")
	btnVectorOtherWeak = vectorMenu.Data("Другая слабая зона", "question_vector", "other_weak")
	btnVectorRandom    = vectorMenu.Data("Случайная тема", "question_vector", "random")
	btnVectorFinish    = vectorMenu.Data("Завершить тренировку", "question_vector", "finish")
)

func init() {
	restartMenu.Inline(restartMenu.Row(btnRestart, btnContinue))
	currentGradeMenu.Inline(currentGradeMenu.Row(btnCurrentGradeJunior, btnCurrentGradeMiddle, btnCurrentGradeSenior))
	targetGradeMenu.Inline(targetGradeMenu.Row(btnTargetGradeJunior, btnTargetGradeMiddle, btnTargetGradeSenior))
	confirmationMenu.Inline(
		confirmationMenu.Row(btnConfirmProfile),
		confirmationMenu.Row(btnEditProfile),
	)
	readyMenu.Inline(readyMenu.Row(btnReady))
	vectorMenu.Inline(
		vectorMenu.Row(btnVectorDeepen),
		vectorMenu.Row(btnVectorOtherWeak),
		vectorMenu.Row(btnVectorRandom),
		vectorMenu.Row(btnVectorFinish),
	)
}

// Repository is the subset of db.Repository this package depends on.
type Repository interface {
	CreateStudentIfAccessCodeValid(ctx context.Context, telegramID int64, name, code string) (*db.Student, error)
	GetStudentByTelegramID(ctx context.Context, telegramID int64) (*db.Student, error)
	GetActiveSession(ctx context.Context, studentID int64) (*db.Session, error)
	StartSession(ctx context.Context, studentID int64) (*db.Session, error)
	EndSession(ctx context.Context, sessionID int64, status string) error
	AdvancePhase(ctx context.Context, sessionID int64, newStatus string) error
	SetQualificationAnswer(ctx context.Context, sessionID int64, step int, answer string) error
	ResetQualification(ctx context.Context, sessionID int64) error
	ConfirmQualification(ctx context.Context, sessionID, studentID int64, topics []string) error
	SaveAuditResults(ctx context.Context, sessionID, studentID int64, topics []string) error
	GetQuestionByID(ctx context.Context, id int64) (*db.QuestionBank, error)
	GetWeakZones(ctx context.Context, studentID int64) ([]db.WeakZone, error)
	StartQuestionAttempt(ctx context.Context, sessionID, questionID int64) (*db.QuestionAttempt, error)
	GetActiveQuestionAttempt(ctx context.Context, sessionID int64) (*db.QuestionAttempt, error)
	SavePrimaryFeedback(ctx context.Context, attemptID int64, answer, feedback, followupQuestion string) error
	MarkPrimaryFeedbackDelivered(ctx context.Context, attemptID int64) error
	SaveFollowupFeedback(ctx context.Context, attemptID int64, answer, feedback, followup2Question string) error
	MarkFollowupFeedbackDelivered(ctx context.Context, attemptID int64) error
	FinalizeQuestionAttempt(ctx context.Context, sessionID, studentID, attemptID int64, answer, miniFeedback, fullFeedback, zoneTopic, zoneStatus string) (int, error)
	MarkFinalFeedbackDelivered(ctx context.Context, attemptID int64) error
	CompleteQuestionAttempt(ctx context.Context, sessionID, attemptID int64, selectedVector, nextTopic string) error
	GetSessionAttemptReports(ctx context.Context, sessionID int64) ([]db.QuestionAttemptReport, error)
	SaveSummary(ctx context.Context, sessionID int64, summaryText string) (*db.SessionSummary, error)
	GetSummaryBySessionID(ctx context.Context, sessionID int64) (*db.SessionSummary, error)
	GetStudentReports(ctx context.Context) ([]db.StudentReport, error)
}

// LLM is the subset of *llm.Service this package depends on.
type LLM interface {
	Reply(ctx context.Context, userMessage string) (*llm.Reply, error)
	Evaluate(ctx context.Context, profile llm.StudentProfile, weakZones []db.WeakZone, studentAnswer string, question *db.QuestionBank) (*llm.Reply, error)
	EvaluateFollowup(ctx context.Context, profile llm.StudentProfile, weakZones []db.WeakZone, primaryAnswer, followupQuestion, followupAnswer string, question *db.QuestionBank) (*llm.Reply, error)
	EvaluateBlock(ctx context.Context, profile llm.StudentProfile, weakZones []db.WeakZone, primaryAnswer, followup1Question, followup1Answer, followup2Question, followup2Answer string, question *db.QuestionBank) (*llm.Reply, error)
	PickQuestion(ctx context.Context, grade, topic string) (*db.QuestionBank, error)
	PickQuestionForSession(ctx context.Context, sessionID int64, grade, topic string) (*db.QuestionBank, error)
}

// Handler groups the dependencies needed to run the interview session
// FSM (INSTRUCTION -> QUALIFICATION -> AUDIT -> QUESTION_CYCLE -> SUMMARY).
type Handler struct {
	repo              Repository
	llm               LLM
	logger            *slog.Logger
	sessionCycleLimit int
	adminIDs          map[int64]bool
	studentLocks      [studentLockStripeCount]sync.Mutex
	lifecycleMu       sync.Mutex
	handlerWG         sync.WaitGroup
	acceptingHandlers bool
}

// New creates a Handler with its dependencies. sessionCycleLimit is the
// same value substituted into the system prompt (SESSION_CYCLE_LIMIT);
// the handler uses its copy to show a continue-or-finish checkpoint after each
// configured number of completed blocks. adminIDs are the Telegram user IDs
// allowed to run /report.
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
		acceptingHandlers: true,
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
	h.handle(bot, &btnCurrentGradeJunior, h.handleGradeCallback)
	h.handle(bot, &btnTargetGradeJunior, h.handleGradeCallback)
	h.handle(bot, &btnConfirmProfile, h.handleConfirmProfileCallback)
	h.handle(bot, &btnEditProfile, h.handleEditProfileCallback)
	h.handle(bot, &btnReady, h.handleReadyCallback)
	h.handle(bot, &btnVectorDeepen, h.handleVectorCallback)
	h.handle(bot, tele.OnText, h.handleMessage)
}

// handle registers fn for endpoint with recoverMiddleware applied
// directly, as a per-call middleware argument to bot.Handle, rather
// than relying solely on the global bot.Use chain. telebot bakes
// whatever middleware is passed here into the stored handler at this
// exact call, so there is no ordering to get wrong and no way for an
// endpoint registered through this helper to accidentally skip it.
func (h *Handler) handle(bot *tele.Bot, endpoint interface{}, fn tele.HandlerFunc) {
	bot.Handle(endpoint, h.trackHandler(fn), h.recoverMiddleware)
}

// trackHandler lets shutdown stop accepting newly scheduled handlers and wait
// for every handler that already started before the database pool is closed.
func (h *Handler) trackHandler(next tele.HandlerFunc) tele.HandlerFunc {
	return func(c tele.Context) error {
		h.lifecycleMu.Lock()
		if !h.acceptingHandlers {
			h.lifecycleMu.Unlock()
			return nil
		}
		h.handlerWG.Add(1)
		h.lifecycleMu.Unlock()

		defer h.handlerWG.Done()
		return next(c)
	}
}

// Shutdown rejects handlers that were scheduled but have not started and
// waits until in-flight handlers finish or ctx expires.
func (h *Handler) Shutdown(ctx context.Context) error {
	h.lifecycleMu.Lock()
	h.acceptingHandlers = false
	h.lifecycleMu.Unlock()

	done := make(chan struct{})
	go func() {
		h.handlerWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

// lockStudent serializes updates from the same Telegram account inside this
// process. Telebot dispatches updates in separate goroutines, so without this
// guard two fast messages can both read the same attempt status and call the
// LLM for the same step. Database constraints remain the final safety net.
func (h *Handler) lockStudent(telegramID int64) func() {
	stripe := &h.studentLocks[uint64(telegramID)%studentLockStripeCount]
	stripe.Lock()
	return stripe.Unlock
}

func newHandlerContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), handlerTimeout)
}

func validateCandidateAnswer(text string) (string, string) {
	answer := strings.TrimSpace(text)
	if answer == "" {
		return "", "Нужен текстовый ответ, чтобы я мог продолжить."
	}
	if utf8.RuneCountInString(answer) > maxCandidateAnswerRunes {
		return "", fmt.Sprintf("Ответ слишком длинный. Сократи его до %d символов.", maxCandidateAnswerRunes)
	}
	return answer, ""
}

// handleStart handles "/start" and "/start CODE". An unregistered
// student must supply a valid code; a registered student with no active
// session gets a fresh one; a registered student with an active session
// is asked whether to restart or continue it.
func (h *Handler) handleStart(c tele.Context) error {
	telegramID := senderID(c)
	unlock := h.lockStudent(telegramID)
	defer unlock()
	ctx, cancel := newHandlerContext()
	defer cancel()

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

	if err := sendPhoto(c, welcomePhotoPath); err != nil {
		h.logger.Error("send welcome photo", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}
	if err := c.Send(instructionMessage); err != nil {
		return err
	}

	if err := h.repo.AdvancePhase(ctx, session.ID, db.SessionStatusQualification); err != nil {
		h.logger.Error("advance phase", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}
	return c.Send(currentGradeQuestion, currentGradeMenu)
}

// handleGradeCallback stores the explicit grade button choice and advances to
// the second qualification question. Text grade answers are also accepted by
// handleQualification for accessibility and recovery.
func (h *Handler) handleGradeCallback(c tele.Context) error {
	if err := c.Respond(); err != nil {
		h.logger.Warn("respond callback", "error", err)
	}

	telegramID := senderID(c)
	unlock := h.lockStudent(telegramID)
	defer unlock()
	ctx, cancel := newHandlerContext()
	defer cancel()
	student, err := h.repo.GetStudentByTelegramID(ctx, telegramID)
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
	if session.Status != db.SessionStatusQualification ||
		(session.QualificationStep != db.QualificationStepCurrentGrade && session.QualificationStep != db.QualificationStepTargetGrade) {
		return c.Send("Этот выбор уже сохранён. Продолжаем с текущего шага.")
	}

	grade, ok := normalizeGrade(c.Data())
	if !ok {
		return c.Send("Выбери грейд кнопкой: джун, мидл или сеньор.", gradeMenuForStep(session.QualificationStep))
	}
	step := session.QualificationStep
	if err := h.repo.SetQualificationAnswer(ctx, session.ID, step, grade); err != nil {
		h.logger.Error("set qualification grade", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}
	if step == db.QualificationStepCurrentGrade {
		return c.Send(targetGradeQuestion, targetGradeMenu)
	}
	return c.Send(strongZonesQuestion)
}

// handleRestartCallback ends the student's active session as abandoned
// and starts a fresh one.
func (h *Handler) handleRestartCallback(c tele.Context) error {
	if err := c.Respond(); err != nil {
		h.logger.Warn("respond callback", "error", err)
	}

	telegramID := senderID(c)
	unlock := h.lockStudent(telegramID)
	defer unlock()
	ctx, cancel := newHandlerContext()
	defer cancel()
	student, err := h.repo.GetStudentByTelegramID(ctx, telegramID)
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

	telegramID := senderID(c)
	unlock := h.lockStudent(telegramID)
	defer unlock()
	ctx, cancel := newHandlerContext()
	defer cancel()
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
		return c.Send("Активной сессии уже нет. Напиши /start и код доступа, чтобы начать заново.")
	}
	if err != nil {
		h.logger.Error("get active session", "error", err)
		return c.Send(genericErrorMessage)
	}

	return h.resumeCurrentStep(ctx, c, session)
}

// handleMessage routes a plain text message to the phase-specific
// handler for the student's currently active session. The session's
// status is always re-read from the database first, per the FSM design:
// there is no in-memory session state.
func (h *Handler) handleMessage(c tele.Context) error {
	telegramID := senderID(c)
	unlock := h.lockStudent(telegramID)
	defer unlock()
	ctx, cancel := newHandlerContext()
	defer cancel()

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
	case db.SessionStatusProfileConfirmation:
		return c.Send("Проверь сохранённые ответы и выбери действие кнопкой.", confirmationMenu)
	case db.SessionStatusKDIRLesson:
		return c.Send("Когда будешь готов, нажми кнопку.", readyMenu)
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
	answer, validationMessage := validateCandidateAnswer(c.Text())
	if validationMessage != "" {
		return c.Send(validationMessage)
	}

	switch session.QualificationStep {
	case db.QualificationStepCurrentGrade:
		grade, ok := normalizeGrade(answer)
		if !ok {
			return c.Send("Выбери грейд: джун, мидл или сеньор.", currentGradeMenu)
		}
		if err := h.repo.SetQualificationAnswer(ctx, session.ID, db.QualificationStepCurrentGrade, grade); err != nil {
			return h.qualificationSaveError(c, session.ID, err)
		}
		return c.Send(targetGradeQuestion, targetGradeMenu)

	case db.QualificationStepTargetGrade:
		grade, ok := normalizeGrade(answer)
		if !ok {
			return c.Send("Выбери грейд: джун, мидл или сеньор.", targetGradeMenu)
		}
		if err := h.repo.SetQualificationAnswer(ctx, session.ID, db.QualificationStepTargetGrade, grade); err != nil {
			return h.qualificationSaveError(c, session.ID, err)
		}
		return c.Send(strongZonesQuestion)

	case db.QualificationStepStrongZones:
		if err := h.repo.SetQualificationAnswer(ctx, session.ID, db.QualificationStepStrongZones, answer); err != nil {
			return h.qualificationSaveError(c, session.ID, err)
		}
		return c.Send(weakZonesQuestion)

	case db.QualificationStepWeakZones:
		if err := h.repo.SetQualificationAnswer(ctx, session.ID, db.QualificationStepWeakZones, answer); err != nil {
			return h.qualificationSaveError(c, session.ID, err)
		}
		session.WeakZonesInput = answer
		session.QualificationStep = db.QualificationStepDone
		if err := h.repo.AdvancePhase(ctx, session.ID, db.SessionStatusProfileConfirmation); err != nil {
			h.logger.Error("advance to profile confirmation", "error", err, "session_id", session.ID)
			return c.Send(genericErrorMessage)
		}
		session.Status = db.SessionStatusProfileConfirmation
		return c.Send(formatQualificationSummary(session), confirmationMenu)

	case db.QualificationStepDone:
		if err := h.repo.AdvancePhase(ctx, session.ID, db.SessionStatusProfileConfirmation); err != nil {
			h.logger.Error("advance completed qualification to confirmation", "error", err, "session_id", session.ID)
			return c.Send(genericErrorMessage)
		}
		session.Status = db.SessionStatusProfileConfirmation
		return c.Send(formatQualificationSummary(session), confirmationMenu)

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

func gradeMenuForStep(step int) *tele.ReplyMarkup {
	if step == db.QualificationStepTargetGrade {
		return targetGradeMenu
	}
	return currentGradeMenu
}

func formatQualificationSummary(session *db.Session) string {
	return fmt.Sprintf(`Зафиксировала твои ответы, правильно ли я понимаю, что...

Текущий грейд: %s
Целевой грейд: %s
Сильные зоны: %s

Получается, в первую очередь будем подтягивать %s для того, чтобы получить долгожданный оффер.
Всё верно?`, session.CurrentGrade, session.Grade, session.StrongZones, session.WeakZonesInput)
}

func qualificationTopics(answer string) []string {
	normalized := strings.ToLower(answer)
	keywords := []struct {
		phrase string
		topic  string
	}{
		{"интеграции", "интеграции"},
		{"базы данных", "бд"},
		{"архитектура", "архитектура"},
		{"требования", "требования"},
		{"безопасность", "безопасность"},
	}

	var topics []string
	for _, keyword := range keywords {
		if strings.Contains(normalized, keyword.phrase) {
			topics = append(topics, keyword.topic)
		}
	}
	return topics
}

func sendPhoto(c tele.Context, path string) error {
	return c.Send(&tele.Photo{File: tele.FromDisk(path)})
}

func (h *Handler) sendKDIRLesson(c tele.Context, sessionID int64) error {
	if err := c.Send(kdirLessonMessage); err != nil {
		return err
	}
	if err := sendPhoto(c, readyPhotoPath); err != nil {
		h.logger.Error("send ready photo", "error", err, "session_id", sessionID)
		return c.Send(genericErrorMessage)
	}
	return c.Send(readyPrompt, readyMenu)
}

func (h *Handler) handleConfirmProfileCallback(c tele.Context) error {
	if err := c.Respond(); err != nil {
		h.logger.Warn("respond callback", "error", err)
	}

	telegramID := senderID(c)
	unlock := h.lockStudent(telegramID)
	defer unlock()
	ctx, cancel := newHandlerContext()
	defer cancel()

	student, err := h.repo.GetStudentByTelegramID(ctx, telegramID)
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

	if session.Status == db.SessionStatusKDIRLesson {
		return h.sendKDIRLesson(c, session.ID)
	}
	if session.Status != db.SessionStatusProfileConfirmation {
		return c.Send("Этот выбор уже сохранён. Продолжаем с текущего шага.")
	}

	topics := qualificationTopics(session.WeakZonesInput)
	if err := h.repo.ConfirmQualification(ctx, session.ID, student.TelegramID, topics); err != nil {
		h.logger.Error("confirm qualification", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}
	return h.sendKDIRLesson(c, session.ID)
}

func (h *Handler) handleEditProfileCallback(c tele.Context) error {
	if err := c.Respond(); err != nil {
		h.logger.Warn("respond callback", "error", err)
	}

	telegramID := senderID(c)
	unlock := h.lockStudent(telegramID)
	defer unlock()
	ctx, cancel := newHandlerContext()
	defer cancel()

	student, err := h.repo.GetStudentByTelegramID(ctx, telegramID)
	if err != nil {
		h.logger.Error("get student for qualification edit", "error", err)
		return c.Send(genericErrorMessage)
	}
	session, err := h.repo.GetActiveSession(ctx, student.TelegramID)
	if err != nil {
		h.logger.Error("get active session for qualification edit", "error", err)
		return c.Send(genericErrorMessage)
	}
	if session.Status != db.SessionStatusProfileConfirmation {
		return c.Send("Анкета уже подтверждена. Продолжаем с текущего шага.")
	}
	if err := h.repo.ResetQualification(ctx, session.ID); err != nil {
		h.logger.Error("reset qualification", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}
	return c.Send(currentGradeQuestion, currentGradeMenu)
}

func (h *Handler) handleReadyCallback(c tele.Context) error {
	if err := c.Respond(); err != nil {
		h.logger.Warn("respond callback", "error", err)
	}

	telegramID := senderID(c)
	unlock := h.lockStudent(telegramID)
	defer unlock()
	ctx, cancel := newHandlerContext()
	defer cancel()

	student, err := h.repo.GetStudentByTelegramID(ctx, telegramID)
	if err != nil {
		h.logger.Error("get student for KDIR ready", "error", err)
		return c.Send(genericErrorMessage)
	}
	session, err := h.repo.GetActiveSession(ctx, student.TelegramID)
	if err != nil {
		h.logger.Error("get active session for KDIR ready", "error", err)
		return c.Send(genericErrorMessage)
	}
	if session.Status != db.SessionStatusKDIRLesson {
		return c.Send("Этот выбор уже сохранён. Продолжаем с текущего шага.")
	}
	if err := h.repo.AdvancePhase(ctx, session.ID, db.SessionStatusQuestionCycle); err != nil {
		h.logger.Error("advance KDIR lesson to question cycle", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}
	session.Status = db.SessionStatusQuestionCycle
	return h.askNextQuestion(ctx, c, student, session)
}

func (h *Handler) resumeCurrentStep(ctx context.Context, c tele.Context, session *db.Session) error {
	switch session.Status {
	case db.SessionStatusInstruction:
		if err := sendPhoto(c, welcomePhotoPath); err != nil {
			return err
		}
		if err := c.Send(instructionMessage); err != nil {
			return err
		}
		if err := h.repo.AdvancePhase(ctx, session.ID, db.SessionStatusQualification); err != nil {
			h.logger.Error("advance resumed instruction", "error", err, "session_id", session.ID)
			return c.Send(genericErrorMessage)
		}
		return c.Send(currentGradeQuestion, currentGradeMenu)
	case db.SessionStatusQualification:
		switch session.QualificationStep {
		case db.QualificationStepCurrentGrade:
			return c.Send(currentGradeQuestion, currentGradeMenu)
		case db.QualificationStepTargetGrade:
			return c.Send(targetGradeQuestion, targetGradeMenu)
		case db.QualificationStepStrongZones:
			return c.Send(strongZonesQuestion)
		case db.QualificationStepWeakZones:
			return c.Send(weakZonesQuestion)
		default:
			return c.Send(formatQualificationSummary(session), confirmationMenu)
		}
	case db.SessionStatusProfileConfirmation:
		return c.Send(formatQualificationSummary(session), confirmationMenu)
	case db.SessionStatusKDIRLesson:
		return h.sendKDIRLesson(c, session.ID)
	default:
		return c.Send("Хорошо, продолжаем с того места, где остановились. Пиши следующее сообщение.")
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
		if err := h.repo.SaveAuditResults(ctx, session.ID, student.TelegramID, topics); err != nil {
			h.logger.Error("save audit results", "error", err, "session_id", session.ID)
			return c.Send(genericErrorMessage)
		}
	}

	if err := sendText(c, cleaned); err != nil {
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

// handleQuestionCycle routes the incoming message using the durable attempt
// status. One block consists of a primary answer, two follow-up answers and a
// vector choice before the next bank question.
func (h *Handler) handleQuestionCycle(ctx context.Context, c tele.Context, student *db.Student, session *db.Session) error {
	attempt, err := h.repo.GetActiveQuestionAttempt(ctx, session.ID)
	if errors.Is(err, db.ErrNoActiveQuestionAttempt) {
		if session.CurrentQuestionID == nil {
			return h.askNextQuestion(ctx, c, student, session)
		}
		// Compatibility with an active session created before migration 00007:
		// preserve the already asked question and begin tracking its answer.
		attempt, err = h.repo.StartQuestionAttempt(ctx, session.ID, *session.CurrentQuestionID)
	}
	if err != nil {
		h.logger.Error("get active question attempt", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}

	switch attempt.Status {
	case db.QuestionAttemptWaitingPrimary:
		return h.evaluatePrimaryAnswer(ctx, c, student, session, attempt)
	case db.QuestionAttemptPrimaryFeedbackReady:
		return h.deliverPrimaryFeedback(ctx, c, attempt)
	case db.QuestionAttemptWaitingFollowup:
		return h.evaluateFollowupAnswer(ctx, c, student, session, attempt)
	case db.QuestionAttemptFollowupFeedbackReady:
		return h.deliverFollowupFeedback(ctx, c, attempt)
	case db.QuestionAttemptWaitingFollowup2:
		return h.evaluateSecondFollowupAnswer(ctx, c, student, session, attempt)
	case db.QuestionAttemptFinalFeedbackReady:
		question, questionErr := h.repo.GetQuestionByID(ctx, attempt.QuestionID)
		if questionErr != nil {
			h.logger.Error("get question for final feedback delivery", "error", questionErr, "attempt_id", attempt.ID)
			return c.Send(genericErrorMessage)
		}
		return h.deliverFinalFeedback(ctx, c, student, session, attempt, question)
	case db.QuestionAttemptWaitingVector:
		question, questionErr := h.repo.GetQuestionByID(ctx, attempt.QuestionID)
		if questionErr != nil {
			return c.Send("Выбери кнопкой, куда двигаться в следующем цикле.", vectorMenu)
		}
		return h.sendVectorChoice(c, session, question)
	default:
		h.logger.Error("unexpected question attempt status", "status", attempt.Status, "attempt_id", attempt.ID)
		return c.Send(genericErrorMessage)
	}
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

	var topic string
	switch {
	case session.NextTopic == "*":
		// An explicit random choice skips all weak-zone priorities once.
		topic = ""
	case validTopics[strings.ToLower(session.NextTopic)]:
		topic = strings.ToLower(session.NextTopic)
	default:
		var err error
		topic, err = h.pickPriorityTopic(ctx, student, session)
		if err != nil {
			h.logger.Warn("pick priority topic", "error", err, "session_id", session.ID)
		}
	}

	question, err := h.llm.PickQuestionForSession(ctx, session.ID, grade, topic)
	if errors.Is(err, db.ErrNoMatchingQuestion) && topic != "" {
		// The priority topic had no matching question for this grade;
		// retry without it rather than dead-ending the cycle.
		question, err = h.llm.PickQuestionForSession(ctx, session.ID, grade, "")
	}
	if errors.Is(err, db.ErrNoMatchingQuestion) {
		if sendErr := c.Send("Подходящие вопросы в банке закончились. Завершаю тренировку и собираю итог."); sendErr != nil {
			return sendErr
		}
		if advanceErr := h.repo.AdvancePhase(ctx, session.ID, db.SessionStatusSummary); advanceErr != nil {
			h.logger.Error("advance exhausted question bank to summary", "error", advanceErr, "session_id", session.ID)
			return c.Send(genericErrorMessage)
		}
		session.Status = db.SessionStatusSummary
		return h.runSummary(ctx, c, student, session)
	}
	if err != nil {
		h.logger.Error("pick question", "error", err, "grade", grade, "topic", topic, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}

	if _, err := h.repo.StartQuestionAttempt(ctx, session.ID, question.ID); err != nil {
		h.logger.Error("start question attempt", "error", err, "session_id", session.ID, "question_id", question.ID)
		return c.Send(genericErrorMessage)
	}
	session.CurrentQuestionID = &question.ID
	session.NextTopic = ""

	return c.Send(formatPrimaryQuestion(question, session.CycleCount == 0))
}

func formatPrimaryQuestion(question *db.QuestionBank, first bool) string {
	lead := "Следующий вопрос, который может прилететь на собеседовании."
	if first {
		lead = "Отлично, первый вопрос, который может прилететь на собеседовании."
	}
	contextText := strings.TrimSpace(question.QuestionContext)
	if contextText == "" {
		contextText = "Представь, что это вопрос с технического собеседования системного аналитика."
	}
	return fmt.Sprintf("%s\n\n%s\n\n%s\n\nОтветь текстом по формуле КДИР. Я разберу ответ и сразу дам обратную связь.",
		lead, contextText, strings.TrimSpace(question.QuestionText))
}

func formatFollowupQuestion(contextText, questionText string, number int) string {
	contextText = strings.TrimSpace(contextText)
	if contextText == "" {
		contextText = "HR хочет проверить, как ты применишь ответ на практике."
	}
	return fmt.Sprintf("Теперь уточняющий вопрос %d из 2.\n\n%s\n\n%s\n\nОтвечай так же текстом по формуле КДИР.",
		number, contextText, strings.TrimSpace(questionText))
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
		if t := strings.ToLower(wz.ZoneText); wz.Status != db.WeakZoneStatusClosed && validTopics[t] {
			return t, nil
		}
	}

	// weak_topics is normally only ever written by SaveAuditResults with
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

func (h *Handler) evaluatePrimaryAnswer(ctx context.Context, c tele.Context, student *db.Student, session *db.Session, attempt *db.QuestionAttempt) error {
	answer, validationMessage := validateCandidateAnswer(c.Text())
	if validationMessage != "" {
		return c.Send(validationMessage)
	}
	question, err := h.repo.GetQuestionByID(ctx, attempt.QuestionID)
	if err != nil {
		h.logger.Error("get primary question", "error", err, "session_id", session.ID, "question_id", attempt.QuestionID)
		return c.Send(genericErrorMessage)
	}

	weakZones, err := h.repo.GetWeakZones(ctx, student.TelegramID)
	if err != nil {
		h.logger.Error("get weak zones", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}

	profile := llm.StudentProfile{
		CurrentGrade: session.CurrentGrade,
		TargetGrade:  session.Grade,
		StrongZones:  session.StrongZones,
		WeakZones:    session.WeakZonesInput,
	}

	reply, err := h.llm.Evaluate(ctx, profile, weakZones, answer, question)
	if err != nil {
		h.logger.Error("llm evaluate primary answer", "error", err, "session_id", session.ID, "question_id", question.ID)
		return c.Send(genericErrorMessage)
	}

	followup := question.Followup1
	followupContext := question.Followup1Context
	if strings.TrimSpace(followup) == "" {
		followup = question.Followup2
		followupContext = question.Followup2Context
	}
	if strings.TrimSpace(followup) == "" {
		followup = "Приведи конкретный пример и объясни, почему выбрал именно такой подход."
	}
	followup = formatFollowupQuestion(followupContext, followup, 1)

	if err := h.repo.SavePrimaryFeedback(ctx, attempt.ID, answer, reply.Text, followup); err != nil {
		h.logger.Error("save primary feedback", "error", err, "attempt_id", attempt.ID)
		return c.Send(genericErrorMessage)
	}

	attempt.PrimaryFeedback = reply.Text
	attempt.FollowupQuestion = followup
	attempt.Status = db.QuestionAttemptPrimaryFeedbackReady
	return h.deliverPrimaryFeedback(ctx, c, attempt)
}

func (h *Handler) deliverPrimaryFeedback(ctx context.Context, c tele.Context, attempt *db.QuestionAttempt) error {
	if err := sendText(c, attempt.PrimaryFeedback); err != nil {
		return err
	}
	if err := c.Send(attempt.FollowupQuestion); err != nil {
		return err
	}
	if err := h.repo.MarkPrimaryFeedbackDelivered(ctx, attempt.ID); err != nil {
		h.logger.Error("mark primary feedback delivered", "error", err, "attempt_id", attempt.ID)
		return c.Send(genericErrorMessage)
	}
	return nil
}

func (h *Handler) evaluateFollowupAnswer(ctx context.Context, c tele.Context, student *db.Student, session *db.Session, attempt *db.QuestionAttempt) error {
	answer, validationMessage := validateCandidateAnswer(c.Text())
	if validationMessage != "" {
		return c.Send(validationMessage)
	}
	question, err := h.repo.GetQuestionByID(ctx, attempt.QuestionID)
	if err != nil {
		h.logger.Error("get followup question", "error", err, "attempt_id", attempt.ID, "question_id", attempt.QuestionID)
		return c.Send(genericErrorMessage)
	}
	weakZones, err := h.repo.GetWeakZones(ctx, student.TelegramID)
	if err != nil {
		h.logger.Error("get weak zones", "error", err, "attempt_id", attempt.ID)
		return c.Send(genericErrorMessage)
	}

	profile := llm.StudentProfile{
		CurrentGrade: session.CurrentGrade,
		TargetGrade:  session.Grade,
		StrongZones:  session.StrongZones,
		WeakZones:    session.WeakZonesInput,
	}
	reply, err := h.llm.EvaluateFollowup(
		ctx, profile, weakZones, attempt.PrimaryAnswer,
		attempt.FollowupQuestion, answer, question,
	)
	if err != nil {
		h.logger.Error("llm evaluate followup answer", "error", err, "attempt_id", attempt.ID)
		return c.Send(genericErrorMessage)
	}

	followup2 := strings.TrimSpace(question.Followup2)
	if followup2 == "" {
		followup2 = "Какой конкретный результат получился и как ты понял, что выбранный подход сработал?"
	}
	followup2 = formatFollowupQuestion(question.Followup2Context, followup2, 2)
	if err := h.repo.SaveFollowupFeedback(ctx, attempt.ID, answer, reply.Text, followup2); err != nil {
		h.logger.Error("save followup feedback", "error", err, "attempt_id", attempt.ID)
		return c.Send(genericErrorMessage)
	}
	attempt.FollowupAnswer = answer
	attempt.FollowupFeedback = reply.Text
	attempt.Followup2Question = followup2
	attempt.Status = db.QuestionAttemptFollowupFeedbackReady
	return h.deliverFollowupFeedback(ctx, c, attempt)
}

func (h *Handler) deliverFollowupFeedback(ctx context.Context, c tele.Context, attempt *db.QuestionAttempt) error {
	if err := sendText(c, attempt.FollowupFeedback); err != nil {
		return err
	}
	if err := c.Send(attempt.Followup2Question); err != nil {
		return err
	}
	if err := h.repo.MarkFollowupFeedbackDelivered(ctx, attempt.ID); err != nil {
		h.logger.Error("mark followup feedback delivered", "error", err, "attempt_id", attempt.ID)
		return c.Send(genericErrorMessage)
	}
	return nil
}

func (h *Handler) evaluateSecondFollowupAnswer(ctx context.Context, c tele.Context, student *db.Student, session *db.Session, attempt *db.QuestionAttempt) error {
	answer, validationMessage := validateCandidateAnswer(c.Text())
	if validationMessage != "" {
		return c.Send(validationMessage)
	}
	question, err := h.repo.GetQuestionByID(ctx, attempt.QuestionID)
	if err != nil {
		h.logger.Error("get question for block evaluation", "error", err, "attempt_id", attempt.ID)
		return c.Send(genericErrorMessage)
	}
	weakZones, err := h.repo.GetWeakZones(ctx, student.TelegramID)
	if err != nil {
		h.logger.Error("get weak zones", "error", err, "attempt_id", attempt.ID)
		return c.Send(genericErrorMessage)
	}
	profile := llm.StudentProfile{
		CurrentGrade: session.CurrentGrade,
		TargetGrade:  session.Grade,
		StrongZones:  session.StrongZones,
		WeakZones:    session.WeakZonesInput,
	}
	reply, err := h.llm.EvaluateBlock(
		ctx, profile, weakZones, attempt.PrimaryAnswer,
		attempt.FollowupQuestion, attempt.FollowupAnswer,
		attempt.Followup2Question, answer, question,
	)
	if err != nil {
		h.logger.Error("llm evaluate complete block", "error", err, "attempt_id", attempt.ID)
		return c.Send(genericErrorMessage)
	}

	zoneStatus, cleaned := extractWeakZoneStatus(reply.Text)
	if zoneStatus == "" {
		zoneStatus = db.WeakZoneStatusHypothesis
	}
	var zoneTopic string
	if validTopics[strings.ToLower(question.Topic)] {
		zoneTopic = strings.ToLower(question.Topic)
	}
	miniFeedback, fullFeedback := splitBlockFeedback(cleaned)

	count, err := h.repo.FinalizeQuestionAttempt(
		ctx, session.ID, student.TelegramID, attempt.ID,
		answer, miniFeedback, fullFeedback, zoneTopic, zoneStatus,
	)
	if err != nil {
		h.logger.Error("finalize question attempt", "error", err, "session_id", session.ID, "attempt_id", attempt.ID)
		return c.Send(genericErrorMessage)
	}
	attempt.Followup2Answer = answer
	attempt.Followup2Feedback = miniFeedback
	attempt.FinalFeedback = fullFeedback
	attempt.Status = db.QuestionAttemptFinalFeedbackReady
	session.CycleCount = count
	return h.deliverFinalFeedback(ctx, c, student, session, attempt, question)
}

func (h *Handler) deliverFinalFeedback(ctx context.Context, c tele.Context, student *db.Student, session *db.Session, attempt *db.QuestionAttempt, question *db.QuestionBank) error {
	if err := sendText(c, attempt.Followup2Feedback); err != nil {
		return err
	}
	if err := sendText(c, attempt.FinalFeedback); err != nil {
		return err
	}
	if err := h.repo.MarkFinalFeedbackDelivered(ctx, attempt.ID); err != nil {
		h.logger.Error("mark final feedback delivered", "error", err, "attempt_id", attempt.ID)
		return c.Send(genericErrorMessage)
	}
	attempt.Status = db.QuestionAttemptWaitingVector

	return h.sendVectorChoice(c, session, question)
}

func (h *Handler) sendVectorChoice(c tele.Context, session *db.Session, question *db.QuestionBank) error {
	if h.sessionCycleLimit > 0 && session.CycleCount > 0 && session.CycleCount%h.sessionCycleLimit == 0 {
		return c.Send("Хочешь продолжить тренировку?", buildCheckpointMenu())
	}
	return c.Send("Куда двигаемся в следующем блоке?", buildVectorMenu(session.WeakTopics, question.Topic))
}

func (h *Handler) handleVectorCallback(c tele.Context) error {
	if err := c.Respond(); err != nil {
		h.logger.Warn("respond vector callback", "error", err)
	}
	telegramID := senderID(c)
	unlock := h.lockStudent(telegramID)
	defer unlock()
	ctx, cancel := newHandlerContext()
	defer cancel()
	student, err := h.repo.GetStudentByTelegramID(ctx, telegramID)
	if err != nil {
		h.logger.Error("get student for vector", "error", err)
		return c.Send(genericErrorMessage)
	}
	session, err := h.repo.GetActiveSession(ctx, student.TelegramID)
	if err != nil {
		h.logger.Error("get session for vector", "error", err)
		return c.Send(genericErrorMessage)
	}
	attempt, err := h.repo.GetActiveQuestionAttempt(ctx, session.ID)
	if err != nil || attempt.Status != db.QuestionAttemptWaitingVector {
		return c.Send("Этот выбор уже неактуален. Продолжаем с текущего шага.")
	}
	question, err := h.repo.GetQuestionByID(ctx, attempt.QuestionID)
	if err != nil {
		h.logger.Error("get question for vector", "error", err, "attempt_id", attempt.ID)
		return c.Send(genericErrorMessage)
	}

	selectedVector := c.Data()
	var nextTopic string
	parts := strings.Split(selectedVector, "|")
	switch parts[0] {
	case "finish":
		if err := h.repo.CompleteQuestionAttempt(ctx, session.ID, attempt.ID, selectedVector, ""); err != nil {
			h.logger.Error("complete question attempt before summary", "error", err, "attempt_id", attempt.ID)
			return c.Send(genericErrorMessage)
		}
		if err := h.repo.AdvancePhase(ctx, session.ID, db.SessionStatusSummary); err != nil {
			h.logger.Error("advance phase", "error", err, "session_id", session.ID)
			return c.Send(genericErrorMessage)
		}
		session.Status = db.SessionStatusSummary
		return h.runSummary(ctx, c, student, session)
	case "continue":
		nextTopic = "*"
	case "topic":
		if len(parts) != 2 || !validTopics[strings.ToLower(parts[1])] {
			return c.Send("Выбери один из предложенных вариантов.", vectorMenu)
		}
		nextTopic = strings.ToLower(parts[1])
	case "deepen":
		nextTopic = strings.ToLower(question.Topic)
	case "other_weak":
		nextTopic = nextWeakTopic(session.WeakTopics, question.Topic)
		if nextTopic == "" {
			nextTopic = "*"
		}
	case "random":
		nextTopic = "*"
	default:
		return c.Send("Выбери один из предложенных вариантов.", vectorMenu)
	}

	if err := h.repo.CompleteQuestionAttempt(ctx, session.ID, attempt.ID, selectedVector, nextTopic); err != nil {
		h.logger.Error("complete question attempt after vector", "error", err, "attempt_id", attempt.ID)
		return c.Send(genericErrorMessage)
	}
	session.CurrentQuestionID = nil
	session.NextTopic = nextTopic
	return h.askNextQuestion(ctx, c, student, session)
}

func buildVectorMenu(weakTopics, currentTopic string) *tele.ReplyMarkup {
	menu := &tele.ReplyMarkup{}
	currentTopic = strings.ToLower(strings.TrimSpace(currentTopic))
	rows := []tele.Row{}
	if validTopics[currentTopic] {
		deepen := menu.Data("Углубиться: "+currentTopic, "question_vector", "topic", currentTopic)
		rows = append(rows, menu.Row(deepen))
	}
	if other := nextWeakTopic(weakTopics, currentTopic); other != "" {
		otherButton := menu.Data("Другая зона: "+other, "question_vector", "topic", other)
		rows = append(rows, menu.Row(otherButton))
	}
	random := menu.Data("Случайная тема", "question_vector", "random")
	rows = append(rows, menu.Row(random))
	finish := menu.Data("Завершить тренировку", "question_vector", "finish")
	rows = append(rows, menu.Row(finish))
	menu.Inline(rows...)
	return menu
}

func buildCheckpointMenu() *tele.ReplyMarkup {
	menu := &tele.ReplyMarkup{}
	continueButton := menu.Data("Продолжить", "question_vector", "continue")
	finishButton := menu.Data("Завершить тренировку", "question_vector", "finish")
	menu.Inline(menu.Row(continueButton), menu.Row(finishButton))
	return menu
}

func nextWeakTopic(weakTopics, currentTopic string) string {
	currentTopic = strings.ToLower(strings.TrimSpace(currentTopic))
	for _, topic := range strings.Split(weakTopics, ",") {
		topic = strings.ToLower(strings.TrimSpace(topic))
		if topic != "" && topic != currentTopic && validTopics[topic] {
			return topic
		}
	}
	return ""
}

// runSummary runs phase 4: ask the model for the final report, save it,
// and close out the session.
func (h *Handler) runSummary(ctx context.Context, c tele.Context, student *db.Student, session *db.Session) error {
	if saved, err := h.repo.GetSummaryBySessionID(ctx, session.ID); err == nil {
		if err := sendText(c, saved.SummaryText); err != nil {
			return err
		}
		if err := h.repo.EndSession(ctx, session.ID, db.SessionStatusCompleted); err != nil {
			h.logger.Error("end session after recovered summary", "error", err, "session_id", session.ID)
		}
		return nil
	} else if !errors.Is(err, db.ErrSummaryNotFound) {
		h.logger.Error("get existing summary", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}

	weakZones, err := h.repo.GetWeakZones(ctx, student.TelegramID)
	if err != nil {
		h.logger.Error("get weak zones", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}

	profile := llm.QualificationProfile{
		CurrentGrade: session.CurrentGrade,
		TargetGrade:  session.Grade,
		StrongZones:  session.StrongZones,
		WeakZones:    session.WeakZonesInput,
	}
	attempts, err := h.repo.GetSessionAttemptReports(ctx, session.ID)
	if err != nil {
		h.logger.Error("get session attempts for summary", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}

	reply, err := h.llm.Reply(ctx, llm.BuildSummaryContext(profile, weakZones, attempts))
	if err != nil {
		h.logger.Error("llm reply (summary)", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}

	if _, err := h.repo.SaveSummary(ctx, session.ID, reply.Text); err != nil {
		h.logger.Error("save summary", "error", err, "session_id", session.ID)
		return c.Send(genericErrorMessage)
	}

	if err := sendText(c, reply.Text); err != nil {
		return err
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

	ctx, cancel := newHandlerContext()
	defer cancel()
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

// extractWeakZoneStatus strips the technical status emitted after the full
// three-answer block evaluation.
func extractWeakZoneStatus(text string) (status, cleaned string) {
	cleaned = text
	m := weakZoneStatusMarkerRE.FindStringSubmatch(text)
	if m == nil {
		return "", strings.TrimSpace(cleaned)
	}
	status = strings.ToLower(strings.TrimSpace(m[1]))
	cleaned = strings.TrimSpace(strings.Replace(cleaned, m[0], "", 1))
	return status, cleaned
}

// splitBlockFeedback separates the third answer's immediate mini-feedback
// from the full KDIR review so Telegram receives them as two distinct frames.
// If the model misses the requested heading, keep its useful response visible
// and add an explicit fallback instead of dropping either required message.
func splitBlockFeedback(text string) (mini, full string) {
	text = strings.TrimSpace(text)
	loc := bigFeedbackHeadingRE.FindStringIndex(text)
	if loc == nil {
		return text, "▸ БОЛЬШАЯ ОБРАТНАЯ СВЯЗЬ\nМодель не разделила мини-разбор и общий разбор блока. Ответ выше сохранён целиком; попробуй следующий блок."
	}
	mini = strings.TrimSpace(text[:loc[0]])
	full = strings.TrimSpace(text[loc[0]:])
	if mini == "" {
		mini = "▸ ОБРАТНАЯ СВЯЗЬ\nОтвет принят и учтён в общем разборе блока."
	}
	return mini, full
}

// sendText splits long model output below Telegram's message limit. Optional
// markup is attached only to the last chunk.
func sendText(c tele.Context, message string, options ...interface{}) error {
	chunks := splitTelegramText(message, telegramTextChunkLimit)
	for i, chunk := range chunks {
		var err error
		if i == len(chunks)-1 {
			err = c.Send(chunk, options...)
		} else {
			err = c.Send(chunk)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func splitTelegramText(message string, limit int) []string {
	message = strings.TrimSpace(message)
	if message == "" {
		return []string{"(пустой ответ модели)"}
	}
	runes := []rune(message)
	var chunks []string
	for len(runes) > limit {
		cut := limit
		for i := limit; i > limit/2; i-- {
			if runes[i-1] == '\n' {
				cut = i - 1
				break
			}
		}
		chunks = append(chunks, strings.TrimSpace(string(runes[:cut])))
		runes = runes[cut:]
		for len(runes) > 0 && runes[0] == '\n' {
			runes = runes[1:]
		}
	}
	if tail := strings.TrimSpace(string(runes)); tail != "" {
		chunks = append(chunks, tail)
	}
	return chunks
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
