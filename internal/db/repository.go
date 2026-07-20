package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrAccessCodeInvalid is returned when a registration code does not
	// exist or has already been used.
	ErrAccessCodeInvalid = errors.New("access code is invalid or already used")

	// ErrStudentAlreadyRegistered is returned when a Telegram user who
	// already has a student row tries to register again.
	ErrStudentAlreadyRegistered = errors.New("student already registered")

	// ErrNoMatchingQuestion is returned when no question_bank row matches
	// the requested grade/topic.
	ErrNoMatchingQuestion = errors.New("no matching question in question bank")

	// ErrStudentNotFound is returned when no student row exists for a
	// given Telegram ID.
	ErrStudentNotFound = errors.New("student not found")

	// ErrNoActiveSession is returned when a student has no session with
	// ended_at still NULL.
	ErrNoActiveSession = errors.New("no active session")

	// ErrQuestionNotFound is returned when no question_bank row exists
	// for a given ID.
	ErrQuestionNotFound = errors.New("question not found")

	// ErrNoActiveQuestionAttempt is returned when a session has no unfinished
	// three-answer question block.
	ErrNoActiveQuestionAttempt = errors.New("no active question attempt")

	// ErrSummaryNotFound is returned when a session has not persisted its
	// final report yet.
	ErrSummaryNotFound = errors.New("session summary not found")
)

// Repository provides data access for the interview bot's PostgreSQL schema.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository creates a Repository backed by pool.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// CreateStudentIfAccessCodeValid registers a student for telegramID using
// code. The access_codes row is locked for the duration of the
// transaction so two concurrent registrations racing on the same code
// cannot both succeed.
func (r *Repository) CreateStudentIfAccessCodeValid(ctx context.Context, telegramID int64, name, code string) (*Student, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var isUsed bool
	err = tx.QueryRow(ctx,
		`SELECT is_used FROM access_codes WHERE code = $1 FOR UPDATE`,
		code,
	).Scan(&isUsed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAccessCodeInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("lock access code: %w", err)
	}
	if isUsed {
		return nil, ErrAccessCodeInvalid
	}

	student := &Student{}
	err = tx.QueryRow(ctx,
		`INSERT INTO students (telegram_id, name, access_code)
		 VALUES ($1, $2, $3)
		 RETURNING telegram_id, name, access_code, created_at`,
		telegramID, name, code,
	).Scan(&student.TelegramID, &student.Name, &student.AccessCode, &student.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrStudentAlreadyRegistered
		}
		return nil, fmt.Errorf("insert student: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE access_codes SET is_used = true, student_id = $1 WHERE code = $2`,
		telegramID, code,
	); err != nil {
		return nil, fmt.Errorf("mark access code used: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return student, nil
}

// StartSession creates a new interview session for studentID, in the
// first FSM phase (INSTRUCTION).
// StartSession always inserts a brand new row: every column that isn't
// student_id/status is listed explicitly here (rather than left to the
// schema's column defaults) so a restart via AdvancePhase+StartSession
// is guaranteed a genuinely clean slate regardless of what the previous
// session for this student had, and regardless of whether some future
// migration changes a column's default.
func (r *Repository) StartSession(ctx context.Context, studentID int64) (*Session, error) {
	s := &Session{}
	err := r.pool.QueryRow(ctx,
		`INSERT INTO sessions (
		     student_id, status, cycle_count,
		     current_grade, grade, student_request, self_assessment, weak_topics, current_question_id,
		     direction, experience, interview_target, qualification_step, next_topic,
		     strong_zones, weak_zones_input
		 )
		 VALUES ($1, $2, 0, '', '', '', '', '', NULL, '', '', '', 0, '', '', '')
		 RETURNING id, student_id, started_at, ended_at, status, cycle_count,
		           current_grade, grade, student_request, self_assessment, weak_topics, current_question_id,
		           direction, experience, interview_target, qualification_step, next_topic,
		           strong_zones, weak_zones_input`,
		studentID, SessionStatusInstruction,
	).Scan(&s.ID, &s.StudentID, &s.StartedAt, &s.EndedAt, &s.Status, &s.CycleCount,
		&s.CurrentGrade, &s.Grade, &s.StudentRequest, &s.SelfAssessment, &s.WeakTopics, &s.CurrentQuestionID,
		&s.Direction, &s.Experience, &s.InterviewTarget, &s.QualificationStep, &s.NextTopic,
		&s.StrongZones, &s.WeakZonesInput)
	if err != nil {
		return nil, fmt.Errorf("start session: %w", err)
	}
	return s, nil
}

// GetStudentByTelegramID looks up a student by their Telegram ID.
func (r *Repository) GetStudentByTelegramID(ctx context.Context, telegramID int64) (*Student, error) {
	s := &Student{}
	err := r.pool.QueryRow(ctx,
		`SELECT telegram_id, name, access_code, created_at FROM students WHERE telegram_id = $1`,
		telegramID,
	).Scan(&s.TelegramID, &s.Name, &s.AccessCode, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStudentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get student: %w", err)
	}
	return s, nil
}

// GetActiveSession returns studentID's session that has not ended yet
// (ended_at IS NULL), if any.
func (r *Repository) GetActiveSession(ctx context.Context, studentID int64) (*Session, error) {
	s := &Session{}
	err := r.pool.QueryRow(ctx,
		`SELECT id, student_id, started_at, ended_at, status, cycle_count,
		        current_grade, grade, student_request, self_assessment, weak_topics, current_question_id,
		        direction, experience, interview_target, qualification_step, next_topic,
		        strong_zones, weak_zones_input
		 FROM sessions
		 WHERE student_id = $1 AND ended_at IS NULL
		 ORDER BY started_at DESC
		 LIMIT 1`,
		studentID,
	).Scan(&s.ID, &s.StudentID, &s.StartedAt, &s.EndedAt, &s.Status, &s.CycleCount,
		&s.CurrentGrade, &s.Grade, &s.StudentRequest, &s.SelfAssessment, &s.WeakTopics, &s.CurrentQuestionID,
		&s.Direction, &s.Experience, &s.InterviewTarget, &s.QualificationStep, &s.NextTopic,
		&s.StrongZones, &s.WeakZonesInput)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoActiveSession
	}
	if err != nil {
		return nil, fmt.Errorf("get active session: %w", err)
	}
	return s, nil
}

// SetQualificationAnswer stores the answer expected at step and advances the
// deterministic qualification flow to the next step. The SQL column is chosen
// in code rather than interpolated from user input.
func (r *Repository) SetQualificationAnswer(ctx context.Context, sessionID int64, step int, answer string) error {
	var query string
	switch step {
	case QualificationStepCurrentGrade:
		query = `UPDATE sessions SET current_grade = $2, qualification_step = 1 WHERE id = $1 AND status = 'QUALIFICATION' AND qualification_step = 0`
	case QualificationStepTargetGrade:
		query = `UPDATE sessions SET grade = $2, qualification_step = 2 WHERE id = $1 AND status = 'QUALIFICATION' AND qualification_step = 1`
	case QualificationStepStrongZones:
		query = `UPDATE sessions SET strong_zones = $2, qualification_step = 3 WHERE id = $1 AND status = 'QUALIFICATION' AND qualification_step = 2`
	case QualificationStepWeakZones:
		query = `UPDATE sessions SET weak_zones_input = $2, qualification_step = 4 WHERE id = $1 AND status = 'QUALIFICATION' AND qualification_step = 3`
	default:
		return fmt.Errorf("set qualification answer: invalid step %d", step)
	}

	tag, err := r.pool.Exec(ctx, query, sessionID, answer)
	if err != nil {
		return fmt.Errorf("set qualification answer: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set qualification answer: session %d is not waiting for step %d", sessionID, step)
	}
	return nil
}

// AdvancePhase moves sessionID to newStatus, resets cycle_count to 0,
// and clears current_question_id, so the per-phase turn caps and
// question tracking in internal/handlers always start from a clean
// slate right after a transition.
func (r *Repository) AdvancePhase(ctx context.Context, sessionID int64, newStatus string) error {
	expectedStatuses := map[string][]string{
		SessionStatusQualification:       {SessionStatusInstruction},
		SessionStatusProfileConfirmation: {SessionStatusQualification},
		SessionStatusQuestionCycle:       {SessionStatusKDIRLesson, SessionStatusAudit},
		SessionStatusSummary:             {SessionStatusQuestionCycle},
	}[newStatus]
	if len(expectedStatuses) == 0 {
		return fmt.Errorf("advance phase: invalid target status %q", newStatus)
	}
	tag, err := r.pool.Exec(ctx,
		`UPDATE sessions
		 SET status = $2, cycle_count = 0, current_question_id = NULL
		 WHERE id = $1 AND status = ANY($3) AND ended_at IS NULL`,
		sessionID, newStatus, expectedStatuses,
	)
	if err != nil {
		return fmt.Errorf("advance phase: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("advance phase: session %d is not in expected statuses %v", sessionID, expectedStatuses)
	}
	return nil
}

// ResetQualification discards an unconfirmed profile and restarts all four
// deterministic questions from the beginning.
func (r *Repository) ResetQualification(ctx context.Context, sessionID int64) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE sessions
		 SET status = $2,
		     current_grade = '', grade = '', strong_zones = '', weak_zones_input = '',
		     weak_topics = '', qualification_step = 0
		 WHERE id = $1 AND status = $3 AND ended_at IS NULL`,
		sessionID, SessionStatusQualification, SessionStatusProfileConfirmation,
	)
	if err != nil {
		return fmt.Errorf("reset qualification: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("reset qualification: session %d is not waiting for confirmation", sessionID)
	}
	return nil
}

// ConfirmQualification persists the deterministic topic mapping, records the
// self-reported weak zones as hypotheses and advances to the KDIR lesson.
func (r *Repository) ConfirmQualification(ctx context.Context, sessionID, studentID int64, topics []string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("confirm qualification begin: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE sessions
		 SET status = $2, weak_topics = $3
		 WHERE id = $1 AND status = $4 AND qualification_step = $5 AND ended_at IS NULL`,
		sessionID, SessionStatusKDIRLesson, strings.Join(topics, ","),
		SessionStatusProfileConfirmation, QualificationStepDone,
	)
	if err != nil {
		return fmt.Errorf("confirm qualification update session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("confirm qualification: session %d is not waiting for confirmation", sessionID)
	}

	for _, topic := range topics {
		if _, err := tx.Exec(ctx,
			`INSERT INTO weak_zones (student_id, zone_text, status)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (student_id, zone_text)
			 DO UPDATE SET status = EXCLUDED.status, updated_at = now()`,
			studentID, topic, WeakZoneStatusHypothesis,
		); err != nil {
			return fmt.Errorf("confirm qualification weak zone %q: %w", topic, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("confirm qualification commit: %w", err)
	}
	return nil
}

// SaveAuditResults atomically stores the audit's topic list and creates or
// refreshes each corresponding weak-zone hypothesis. The question cycle must
// not start with only half of this state persisted.
func (r *Repository) SaveAuditResults(ctx context.Context, sessionID, studentID int64, topics []string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("save audit results begin: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE sessions SET weak_topics = $2 WHERE id = $1 AND status = $3`,
		sessionID, strings.Join(topics, ","), SessionStatusAudit,
	)
	if err != nil {
		return fmt.Errorf("save audit results update session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("save audit results: session %d is not in audit", sessionID)
	}

	for _, topic := range topics {
		if _, err := tx.Exec(ctx,
			`INSERT INTO weak_zones (student_id, zone_text, status)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (student_id, zone_text)
			 DO UPDATE SET status = EXCLUDED.status, updated_at = now()`,
			studentID, topic, WeakZoneStatusHypothesis,
		); err != nil {
			return fmt.Errorf("save audit weak zone %q: %w", topic, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("save audit results commit: %w", err)
	}
	return nil
}

// EndSession marks sessionID as finished with the given status
// (e.g. "completed", "abandoned") and stamps ended_at.
func (r *Repository) EndSession(ctx context.Context, sessionID int64, status string) error {
	if status != SessionStatusCompleted && status != SessionStatusAbandoned {
		return fmt.Errorf("end session: invalid terminal status %q", status)
	}
	tag, err := r.pool.Exec(ctx,
		`UPDATE sessions SET status = $2, ended_at = now() WHERE id = $1 AND ended_at IS NULL`,
		sessionID, status,
	)
	if err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("end session: session %d is not active", sessionID)
	}
	return nil
}

// SaveWeakZone records or updates a weak zone for a student. Calling it
// again with the same zoneText (e.g. moving it from "hypothesis" to
// "confirmed") updates the existing row instead of creating a duplicate.
func (r *Repository) SaveWeakZone(ctx context.Context, studentID int64, zoneText, status string) (*WeakZone, error) {
	wz := &WeakZone{}
	err := r.pool.QueryRow(ctx,
		`INSERT INTO weak_zones (student_id, zone_text, status)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (student_id, zone_text)
		 DO UPDATE SET status = EXCLUDED.status, updated_at = now()
		 RETURNING id, student_id, zone_text, status, updated_at`,
		studentID, zoneText, status,
	).Scan(&wz.ID, &wz.StudentID, &wz.ZoneText, &wz.Status, &wz.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("save weak zone: %w", err)
	}
	return wz, nil
}

// GetWeakZones returns all weak zones tracked for studentID, most
// recently updated first.
func (r *Repository) GetWeakZones(ctx context.Context, studentID int64) ([]WeakZone, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, student_id, zone_text, status, updated_at
		 FROM weak_zones
		 WHERE student_id = $1
		 ORDER BY updated_at DESC`,
		studentID,
	)
	if err != nil {
		return nil, fmt.Errorf("get weak zones: %w", err)
	}
	defer rows.Close()

	var zones []WeakZone
	for rows.Next() {
		var wz WeakZone
		if err := rows.Scan(&wz.ID, &wz.StudentID, &wz.ZoneText, &wz.Status, &wz.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan weak zone: %w", err)
		}
		zones = append(zones, wz)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate weak zones: %w", err)
	}

	return zones, nil
}

// PickQuestion returns a random question_bank row for grade, optionally
// narrowed down to topic. Grade compatibility is an explicit mapping rather
// than a substring match, so adding a new grade name cannot silently alter
// which questions existing candidates receive.
func (r *Repository) PickQuestion(ctx context.Context, grade, topic string) (*QuestionBank, error) {
	query := `
		SELECT id, question_text, question_context, grade, topic,
		       followup_1, followup_1_context, followup_2, followup_2_context,
		       answer_junior, answer_middle, answer_senior, source
		FROM question_bank
		WHERE grade = ANY($1)`
	args := []any{compatibleQuestionGrades(grade)}

	if topic != "" {
		query += ` AND topic = $2`
		args = append(args, topic)
	}
	query += ` ORDER BY random() LIMIT 1`

	q := &QuestionBank{}
	err := r.pool.QueryRow(ctx, query, args...).Scan(
		&q.ID, &q.QuestionText, &q.QuestionContext, &q.Grade, &q.Topic,
		&q.Followup1, &q.Followup1Context, &q.Followup2, &q.Followup2Context,
		&q.AnswerJunior, &q.AnswerMiddle, &q.AnswerSenior, &q.Source,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoMatchingQuestion
	}
	if err != nil {
		return nil, fmt.Errorf("pick question: %w", err)
	}

	return q, nil
}

// PickQuestionForSession excludes every question already used by the session.
// Topic remains optional and callers may retry without it when a narrow pool is
// exhausted.
func (r *Repository) PickQuestionForSession(ctx context.Context, sessionID int64, grade, topic string) (*QuestionBank, error) {
	query := `
		SELECT q.id, q.question_text, q.question_context, q.grade, q.topic,
		       q.followup_1, q.followup_1_context, q.followup_2, q.followup_2_context,
		       q.answer_junior, q.answer_middle, q.answer_senior, q.source
		FROM question_bank q
		WHERE q.grade = ANY($2)
		  AND NOT EXISTS (
		      SELECT 1 FROM question_attempts a
		      WHERE a.session_id = $1 AND a.question_id = q.id
		  )`
	args := []any{sessionID, compatibleQuestionGrades(grade)}
	if topic != "" {
		query += ` AND q.topic = $3`
		args = append(args, topic)
	}
	query += ` ORDER BY random() LIMIT 1`

	q := &QuestionBank{}
	err := r.pool.QueryRow(ctx, query, args...).Scan(
		&q.ID, &q.QuestionText, &q.QuestionContext, &q.Grade, &q.Topic,
		&q.Followup1, &q.Followup1Context, &q.Followup2, &q.Followup2Context,
		&q.AnswerJunior, &q.AnswerMiddle, &q.AnswerSenior, &q.Source,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoMatchingQuestion
	}
	if err != nil {
		return nil, fmt.Errorf("pick unused question for session: %w", err)
	}
	return q, nil
}

func compatibleQuestionGrades(grade string) []string {
	switch strings.ToLower(strings.TrimSpace(grade)) {
	case "джун":
		return []string{"джун", "джун-мидл"}
	case "джун-мидл":
		return []string{"джун", "джун-мидл", "мидл"}
	case "мидл":
		return []string{"джун-мидл", "мидл", "мидл-сеньор"}
	case "мидл-сеньор":
		return []string{"мидл", "мидл-сеньор", "сеньор"}
	case "сеньор":
		return []string{"мидл-сеньор", "сеньор"}
	default:
		return []string{grade}
	}
}

// GetQuestionByID returns the exact question_bank row for id, used to
// grade a student's answer against the same question that was actually
// asked (see sessions.current_question_id).
func (r *Repository) GetQuestionByID(ctx context.Context, id int64) (*QuestionBank, error) {
	q := &QuestionBank{}
	err := r.pool.QueryRow(ctx,
		`SELECT id, question_text, question_context, grade, topic,
		        followup_1, followup_1_context, followup_2, followup_2_context,
		        answer_junior, answer_middle, answer_senior, source
		 FROM question_bank
		 WHERE id = $1`,
		id,
	).Scan(
		&q.ID, &q.QuestionText, &q.QuestionContext, &q.Grade, &q.Topic,
		&q.Followup1, &q.Followup1Context, &q.Followup2, &q.Followup2Context,
		&q.AnswerJunior, &q.AnswerMiddle, &q.AnswerSenior, &q.Source,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrQuestionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get question by id: %w", err)
	}
	return q, nil
}

// StartQuestionAttempt atomically points the session at questionID and creates
// the durable three-answer block that will receive the student's next messages.
func (r *Repository) StartQuestionAttempt(ctx context.Context, sessionID, questionID int64) (*QuestionAttempt, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("start question attempt begin: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE sessions
		 SET current_question_id = $2, next_topic = ''
		 WHERE id = $1 AND status = $3 AND ended_at IS NULL`,
		sessionID, questionID, SessionStatusQuestionCycle,
	)
	if err != nil {
		return nil, fmt.Errorf("start question attempt update session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("start question attempt: session %d is not in question cycle", sessionID)
	}

	a := &QuestionAttempt{}
	err = tx.QueryRow(ctx,
		`INSERT INTO question_attempts (session_id, question_id)
		 VALUES ($1, $2)
		 RETURNING id, session_id, question_id, primary_answer, primary_feedback,
		           followup_question, followup_answer, followup_feedback,
		           followup_2_question, followup_2_answer, followup_2_feedback,
		           final_feedback, selected_vector,
		           status, created_at, updated_at`,
		sessionID, questionID,
	).Scan(
		&a.ID, &a.SessionID, &a.QuestionID, &a.PrimaryAnswer, &a.PrimaryFeedback,
		&a.FollowupQuestion, &a.FollowupAnswer, &a.FollowupFeedback,
		&a.Followup2Question, &a.Followup2Answer, &a.Followup2Feedback,
		&a.FinalFeedback, &a.SelectedVector,
		&a.Status, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("start question attempt insert: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("start question attempt commit: %w", err)
	}
	return a, nil
}

// GetActiveQuestionAttempt returns the unfinished attempt for sessionID.
func (r *Repository) GetActiveQuestionAttempt(ctx context.Context, sessionID int64) (*QuestionAttempt, error) {
	a := &QuestionAttempt{}
	err := r.pool.QueryRow(ctx,
		`SELECT id, session_id, question_id, primary_answer, primary_feedback,
		        followup_question, followup_answer, followup_feedback,
		        followup_2_question, followup_2_answer, followup_2_feedback,
		        final_feedback, selected_vector,
		        status, created_at, updated_at
		 FROM question_attempts
		 WHERE session_id = $1 AND status <> $2
		 ORDER BY id DESC
		 LIMIT 1`,
		sessionID, QuestionAttemptCompleted,
	).Scan(
		&a.ID, &a.SessionID, &a.QuestionID, &a.PrimaryAnswer, &a.PrimaryFeedback,
		&a.FollowupQuestion, &a.FollowupAnswer, &a.FollowupFeedback,
		&a.Followup2Question, &a.Followup2Answer, &a.Followup2Feedback,
		&a.FinalFeedback, &a.SelectedVector,
		&a.Status, &a.CreatedAt, &a.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoActiveQuestionAttempt
	}
	if err != nil {
		return nil, fmt.Errorf("get active question attempt: %w", err)
	}
	return a, nil
}

// SavePrimaryFeedback stores the first answer and marks its outbound feedback
// ready. The attempt starts waiting for the student's follow-up only after both
// Telegram messages have actually been delivered.
func (r *Repository) SavePrimaryFeedback(ctx context.Context, attemptID int64, answer, feedback, followupQuestion string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE question_attempts
		 SET primary_answer = $2, primary_feedback = $3, followup_question = $4,
		     status = $5, updated_at = now()
		 WHERE id = $1 AND status = $6`,
		attemptID, answer, feedback, followupQuestion,
		QuestionAttemptPrimaryFeedbackReady, QuestionAttemptWaitingPrimary,
	)
	if err != nil {
		return fmt.Errorf("save primary feedback: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("save primary feedback: attempt %d is not waiting for a primary answer", attemptID)
	}
	return nil
}

// MarkPrimaryFeedbackDelivered moves a persisted outbound feedback/follow-up
// pair to the state that consumes the student's next message.
func (r *Repository) MarkPrimaryFeedbackDelivered(ctx context.Context, attemptID int64) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE question_attempts
		 SET status = $2, updated_at = now()
		 WHERE id = $1 AND status = $3`,
		attemptID, QuestionAttemptWaitingFollowup, QuestionAttemptPrimaryFeedbackReady,
	)
	if err != nil {
		return fmt.Errorf("mark primary feedback delivered: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("mark primary feedback delivered: attempt %d is not ready", attemptID)
	}
	return nil
}

// SaveFollowupFeedback stores the second answer and its mini-feedback. The
// second follow-up becomes consumable only after both outbound messages arrive.
func (r *Repository) SaveFollowupFeedback(ctx context.Context, attemptID int64, answer, feedback, followup2Question string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE question_attempts
		 SET followup_answer = $2, followup_feedback = $3, followup_2_question = $4,
		     status = $5, updated_at = now()
		 WHERE id = $1 AND status = $6`,
		attemptID, answer, feedback, followup2Question,
		QuestionAttemptFollowupFeedbackReady, QuestionAttemptWaitingFollowup,
	)
	if err != nil {
		return fmt.Errorf("save followup feedback: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("save followup feedback: attempt %d is not waiting for a follow-up answer", attemptID)
	}
	return nil
}

func (r *Repository) MarkFollowupFeedbackDelivered(ctx context.Context, attemptID int64) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE question_attempts SET status = $2, updated_at = now()
		 WHERE id = $1 AND status = $3`,
		attemptID, QuestionAttemptWaitingFollowup2, QuestionAttemptFollowupFeedbackReady,
	)
	if err != nil {
		return fmt.Errorf("mark followup feedback delivered: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("mark followup feedback delivered: attempt %d is not ready", attemptID)
	}
	return nil
}

// FinalizeQuestionAttempt atomically stores the third answer, its mini-feedback
// and the full block feedback,
// updates the evidence-backed weak-zone status, and increments the count of
// fully evaluated cycles.
func (r *Repository) FinalizeQuestionAttempt(ctx context.Context, sessionID, studentID, attemptID int64, answer, miniFeedback, fullFeedback, zoneTopic, zoneStatus string) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("finalize question attempt begin: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE question_attempts
		 SET followup_2_answer = $2, followup_2_feedback = $3, final_feedback = $4,
		     status = $5, updated_at = now()
		 WHERE id = $1 AND session_id = $6 AND status = $7`,
		attemptID, answer, miniFeedback, fullFeedback, QuestionAttemptFinalFeedbackReady,
		sessionID, QuestionAttemptWaitingFollowup2,
	)
	if err != nil {
		return 0, fmt.Errorf("finalize question attempt update attempt: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, fmt.Errorf("finalize question attempt: attempt %d is not waiting for the second follow-up answer", attemptID)
	}

	if zoneTopic != "" {
		if _, err := tx.Exec(ctx,
			`INSERT INTO weak_zones (student_id, zone_text, status)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (student_id, zone_text)
			 DO UPDATE SET status = EXCLUDED.status, updated_at = now()`,
			studentID, zoneTopic, zoneStatus,
		); err != nil {
			return 0, fmt.Errorf("finalize question attempt weak zone: %w", err)
		}
	}

	var count int
	if err := tx.QueryRow(ctx,
		`UPDATE sessions
		 SET cycle_count = cycle_count + 1
		 WHERE id = $1 AND status = $2
		 RETURNING cycle_count`,
		sessionID, SessionStatusQuestionCycle,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("finalize question attempt increment cycle: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("finalize question attempt commit: %w", err)
	}
	return count, nil
}

// MarkFinalFeedbackDelivered advances the attempt only after the full feedback
// reached Telegram. A failed send leaves it ready for deterministic resend.
func (r *Repository) MarkFinalFeedbackDelivered(ctx context.Context, attemptID int64) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE question_attempts
		 SET status = $2, updated_at = now()
		 WHERE id = $1 AND status = $3`,
		attemptID, QuestionAttemptWaitingVector, QuestionAttemptFinalFeedbackReady,
	)
	if err != nil {
		return fmt.Errorf("mark final feedback delivered: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("mark final feedback delivered: attempt %d is not ready", attemptID)
	}
	return nil
}

// CompleteQuestionAttempt records the vector choice and prepares the session
// for its next question. nextTopic is a concrete bank topic, "*" for random,
// or empty to return to automatic priority selection.
func (r *Repository) CompleteQuestionAttempt(ctx context.Context, sessionID, attemptID int64, selectedVector, nextTopic string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("complete question attempt begin: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE question_attempts
		 SET selected_vector = $2, status = $3, updated_at = now()
		 WHERE id = $1 AND status = $4`,
		attemptID, selectedVector, QuestionAttemptCompleted, QuestionAttemptWaitingVector,
	)
	if err != nil {
		return fmt.Errorf("complete question attempt update attempt: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("complete question attempt: attempt %d is not waiting for a vector", attemptID)
	}

	tag, err = tx.Exec(ctx,
		`UPDATE sessions
		 SET current_question_id = NULL, next_topic = $2
		 WHERE id = $1 AND status = $3 AND ended_at IS NULL`,
		sessionID, nextTopic, SessionStatusQuestionCycle,
	)
	if err != nil {
		return fmt.Errorf("complete question attempt update session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("complete question attempt: session %d is not in question cycle", sessionID)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("complete question attempt commit: %w", err)
	}
	return nil
}

// GetSessionAttemptReports returns completed cycles in chronological order for
// the final report prompt.
func (r *Repository) GetSessionAttemptReports(ctx context.Context, sessionID int64) ([]QuestionAttemptReport, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT q.question_text, q.topic, a.primary_answer, a.followup_question,
		        a.followup_answer, a.followup_2_question, a.followup_2_answer,
		        a.final_feedback
		 FROM question_attempts a
		 JOIN question_bank q ON q.id = a.question_id
		 WHERE a.session_id = $1 AND a.status = $2
		 ORDER BY a.id`,
		sessionID, QuestionAttemptCompleted,
	)
	if err != nil {
		return nil, fmt.Errorf("get session attempt reports: %w", err)
	}
	defer rows.Close()

	var reports []QuestionAttemptReport
	for rows.Next() {
		var report QuestionAttemptReport
		if err := rows.Scan(
			&report.QuestionText, &report.Topic, &report.PrimaryAnswer,
			&report.FollowupQuestion, &report.FollowupAnswer,
			&report.Followup2Question, &report.Followup2Answer, &report.FinalFeedback,
		); err != nil {
			return nil, fmt.Errorf("scan session attempt report: %w", err)
		}
		reports = append(reports, report)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session attempt reports: %w", err)
	}
	return reports, nil
}

// GetStudentReports returns an activity summary for every registered
// student (completed session count, last session date, current
// weak-zone map), ordered by telegram_id. Used by the admin /report
// command.
func (r *Repository) GetStudentReports(ctx context.Context) ([]StudentReport, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT s.telegram_id, s.name,
		        COUNT(sess.id) FILTER (WHERE sess.status = $1) AS completed_sessions,
		        MAX(sess.started_at) AS last_session_at
		 FROM students s
		 LEFT JOIN sessions sess ON sess.student_id = s.telegram_id
		 GROUP BY s.telegram_id, s.name
		 ORDER BY s.telegram_id`,
		SessionStatusCompleted,
	)
	if err != nil {
		return nil, fmt.Errorf("get student reports: %w", err)
	}

	var reports []StudentReport
	for rows.Next() {
		var rep StudentReport
		if err := rows.Scan(&rep.TelegramID, &rep.Name, &rep.CompletedSessions, &rep.LastSessionAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan student report: %w", err)
		}
		reports = append(reports, rep)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate student reports: %w", err)
	}
	rows.Close()

	weakZonesByStudent, err := r.getAllWeakZones(ctx)
	if err != nil {
		return nil, err
	}
	for i := range reports {
		reports[i].WeakZones = weakZonesByStudent[reports[i].TelegramID]
	}

	return reports, nil
}

// getAllWeakZones returns every weak zone grouped by student_id (most
// recently updated first within each student), for GetStudentReports.
func (r *Repository) getAllWeakZones(ctx context.Context) (map[int64][]WeakZone, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, student_id, zone_text, status, updated_at
		 FROM weak_zones
		 ORDER BY student_id, updated_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("get all weak zones: %w", err)
	}
	defer rows.Close()

	result := make(map[int64][]WeakZone)
	for rows.Next() {
		var wz WeakZone
		if err := rows.Scan(&wz.ID, &wz.StudentID, &wz.ZoneText, &wz.Status, &wz.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan weak zone: %w", err)
		}
		result[wz.StudentID] = append(result[wz.StudentID], wz)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate weak zones: %w", err)
	}

	return result, nil
}

// SaveSummary stores the LLM-generated summary for a finished session.
func (r *Repository) SaveSummary(ctx context.Context, sessionID int64, summaryText string) (*SessionSummary, error) {
	s := &SessionSummary{}
	err := r.pool.QueryRow(ctx,
		`INSERT INTO session_summaries (session_id, summary_text)
		 VALUES ($1, $2)
		 ON CONFLICT (session_id)
		 DO UPDATE SET summary_text = EXCLUDED.summary_text, created_at = now()
		 RETURNING id, session_id, summary_text, created_at`,
		sessionID, summaryText,
	).Scan(&s.ID, &s.SessionID, &s.SummaryText, &s.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("save summary: %w", err)
	}
	return s, nil
}

// GetSummaryBySessionID returns a previously generated final report. Summary
// recovery uses it after a Telegram send failure instead of paying for and
// potentially changing a second LLM response.
func (r *Repository) GetSummaryBySessionID(ctx context.Context, sessionID int64) (*SessionSummary, error) {
	s := &SessionSummary{}
	err := r.pool.QueryRow(ctx,
		`SELECT id, session_id, summary_text, created_at
		 FROM session_summaries
		 WHERE session_id = $1`,
		sessionID,
	).Scan(&s.ID, &s.SessionID, &s.SummaryText, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSummaryNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get session summary: %w", err)
	}
	return s, nil
}
