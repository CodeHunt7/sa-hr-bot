package db

import (
	"context"
	"errors"
	"fmt"

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
		     current_grade, grade, student_request, self_assessment, weak_topics, current_question_id
		 )
		 VALUES ($1, $2, 0, '', '', '', '', '', NULL)
		 RETURNING id, student_id, started_at, ended_at, status, cycle_count,
		           current_grade, grade, student_request, self_assessment, weak_topics, current_question_id`,
		studentID, SessionStatusInstruction,
	).Scan(&s.ID, &s.StudentID, &s.StartedAt, &s.EndedAt, &s.Status, &s.CycleCount,
		&s.CurrentGrade, &s.Grade, &s.StudentRequest, &s.SelfAssessment, &s.WeakTopics, &s.CurrentQuestionID)
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
		        current_grade, grade, student_request, self_assessment, weak_topics, current_question_id
		 FROM sessions
		 WHERE student_id = $1 AND ended_at IS NULL
		 ORDER BY started_at DESC
		 LIMIT 1`,
		studentID,
	).Scan(&s.ID, &s.StudentID, &s.StartedAt, &s.EndedAt, &s.Status, &s.CycleCount,
		&s.CurrentGrade, &s.Grade, &s.StudentRequest, &s.SelfAssessment, &s.WeakTopics, &s.CurrentQuestionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoActiveSession
	}
	if err != nil {
		return nil, fmt.Errorf("get active session: %w", err)
	}
	return s, nil
}

// AdvancePhase moves sessionID to newStatus, resets cycle_count to 0,
// and clears current_question_id, so the per-phase turn caps and
// question tracking in internal/handlers always start from a clean
// slate right after a transition.
func (r *Repository) AdvancePhase(ctx context.Context, sessionID int64, newStatus string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE sessions SET status = $2, cycle_count = 0, current_question_id = NULL WHERE id = $1`,
		sessionID, newStatus,
	)
	if err != nil {
		return fmt.Errorf("advance phase: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("advance phase: session %d not found", sessionID)
	}
	return nil
}

// SetCurrentQuestionID sets sessionID's current_question_id, or clears
// it when questionID is nil.
func (r *Repository) SetCurrentQuestionID(ctx context.Context, sessionID int64, questionID *int64) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE sessions SET current_question_id = $2 WHERE id = $1`,
		sessionID, questionID,
	)
	if err != nil {
		return fmt.Errorf("set current question id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set current question id: session %d not found", sessionID)
	}
	return nil
}

// IncrementCycleCount increments sessionID's cycle_count by one and
// returns the new value.
func (r *Repository) IncrementCycleCount(ctx context.Context, sessionID int64) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx,
		`UPDATE sessions SET cycle_count = cycle_count + 1 WHERE id = $1 RETURNING cycle_count`,
		sessionID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("increment cycle count: %w", err)
	}
	return count, nil
}

// SetQualificationFields updates whichever of sessionID's four
// QUALIFICATION fields are non-empty (current grade, target grade,
// student request, self-assessment). An empty argument leaves its
// column unchanged, since the model reveals each field on whichever
// turn it becomes confident about it, not necessarily the same turn.
func (r *Repository) SetQualificationFields(ctx context.Context, sessionID int64, currentGrade, targetGrade, studentRequest, selfAssessment string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE sessions
		 SET current_grade = CASE WHEN $2 = '' THEN current_grade ELSE $2 END,
		     grade = CASE WHEN $3 = '' THEN grade ELSE $3 END,
		     student_request = CASE WHEN $4 = '' THEN student_request ELSE $4 END,
		     self_assessment = CASE WHEN $5 = '' THEN self_assessment ELSE $5 END
		 WHERE id = $1`,
		sessionID, currentGrade, targetGrade, studentRequest, selfAssessment,
	)
	if err != nil {
		return fmt.Errorf("set qualification fields: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set qualification fields: session %d not found", sessionID)
	}
	return nil
}

// SetWeakTopics sets sessionID's weak_topics to a comma-separated list
// of question_bank.topic values, extracted from the mini-audit (AUDIT
// phase) and used as a priority filter in QUESTION_CYCLE's PickQuestion
// calls.
func (r *Repository) SetWeakTopics(ctx context.Context, sessionID int64, topics string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE sessions SET weak_topics = $2 WHERE id = $1`,
		sessionID, topics,
	)
	if err != nil {
		return fmt.Errorf("set weak topics: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set weak topics: session %d not found", sessionID)
	}
	return nil
}

// EndSession marks sessionID as finished with the given status
// (e.g. "completed", "abandoned") and stamps ended_at.
func (r *Repository) EndSession(ctx context.Context, sessionID int64, status string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE sessions SET status = $2, ended_at = now() WHERE id = $1`,
		sessionID, status,
	)
	if err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("end session: session %d not found", sessionID)
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
// narrowed down to topic. Grade matching is a substring check rather than
// equality: bridge grades like "джун-мидл" contain "джун", so a "джун"
// candidate is also offered questions tagged for the bridge grade (and
// likewise "мидл" candidates get "джун-мидл" and "мидл-сеньор" ones).
func (r *Repository) PickQuestion(ctx context.Context, grade, topic string) (*QuestionBank, error) {
	query := `
		SELECT id, question_text, grade, topic, followup_1, followup_2,
		       answer_junior, answer_middle, answer_senior, source
		FROM question_bank
		WHERE grade LIKE '%' || $1 || '%'`
	args := []any{grade}

	if topic != "" {
		query += ` AND topic = $2`
		args = append(args, topic)
	}
	query += ` ORDER BY random() LIMIT 1`

	q := &QuestionBank{}
	err := r.pool.QueryRow(ctx, query, args...).Scan(
		&q.ID, &q.QuestionText, &q.Grade, &q.Topic, &q.Followup1, &q.Followup2,
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

// GetQuestionByID returns the exact question_bank row for id, used to
// grade a student's answer against the same question that was actually
// asked (see sessions.current_question_id).
func (r *Repository) GetQuestionByID(ctx context.Context, id int64) (*QuestionBank, error) {
	q := &QuestionBank{}
	err := r.pool.QueryRow(ctx,
		`SELECT id, question_text, grade, topic, followup_1, followup_2,
		        answer_junior, answer_middle, answer_senior, source
		 FROM question_bank
		 WHERE id = $1`,
		id,
	).Scan(
		&q.ID, &q.QuestionText, &q.Grade, &q.Topic, &q.Followup1, &q.Followup2,
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
		 RETURNING id, session_id, summary_text, created_at`,
		sessionID, summaryText,
	).Scan(&s.ID, &s.SessionID, &s.SummaryText, &s.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("save summary: %w", err)
	}
	return s, nil
}
