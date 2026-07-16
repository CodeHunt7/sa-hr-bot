package db

// Why PostgreSQL:
//   - Multiple students go through interview sessions concurrently; the bot
//     runs many goroutines (one per update) writing session/message state
//     at the same time. PostgreSQL handles concurrent writers safely,
//     unlike SQLite which serializes writes at the file level.
//   - goose migrations, JSONB columns (for flexible LLM message payloads)
//     and standard SQL tooling are all first-class.
//   - Easy to run in Docker/Docker Compose alongside the bot for local dev
//     and to move to a managed instance (RDS, Cloud SQL, etc.) in production
//     without changing the driver.

import "time"

// Weak zone status values.
const (
	WeakZoneStatusHypothesis = "hypothesis"
	WeakZoneStatusConfirmed  = "confirmed"
)

// Session status values. The first five are the interview FSM phases,
// walked in order by internal/handlers; the last two are terminal
// states set by EndSession once a session stops being active.
const (
	SessionStatusInstruction   = "INSTRUCTION"
	SessionStatusQualification = "QUALIFICATION"
	SessionStatusAudit         = "AUDIT"
	SessionStatusQuestionCycle = "QUESTION_CYCLE"
	SessionStatusSummary       = "SUMMARY"
	SessionStatusCompleted     = "COMPLETED"
	SessionStatusAbandoned     = "ABANDONED"
)

// Student is a course participant identified by their Telegram account.
type Student struct {
	TelegramID int64     `db:"telegram_id"`
	Name       string    `db:"name"`
	AccessCode string    `db:"access_code"`
	CreatedAt  time.Time `db:"created_at"`
}

// AccessCode is a single-use invite code that gates bot registration.
type AccessCode struct {
	Code      string `db:"code"`
	IsUsed    bool   `db:"is_used"`
	StudentID *int64 `db:"student_id"`
}

// Session is one mock-interview run for a student.
//
// CurrentGrade, Grade (target grade), StudentRequest and SelfAssessment
// are the four QUALIFICATION fields, filled in as the model reports them
// (see internal/handlers' marker parsing). Grade is also the one used by
// PickQuestion for question difficulty throughout the rest of the
// session. WeakTopics is a comma-separated list of question_bank.topic
// values the mini-audit (AUDIT phase) flags as likely weak, used as a
// priority filter in QUESTION_CYCLE.
// CycleCount is reset to 0 on every phase transition and counts turns
// spent in the current phase; in QUESTION_CYCLE it is compared against
// SESSION_CYCLE_LIMIT to decide when to move to SUMMARY, and in
// QUALIFICATION it is an emergency ceiling in case the model never
// manages to extract all four fields.
// CurrentQuestionID is nil between cycles and set to the question_bank
// row the student was just asked; the next incoming message is graded
// against exactly that question (see internal/handlers' QUESTION_CYCLE
// handling), then it is cleared again.
type Session struct {
	ID                int64      `db:"id"`
	StudentID         int64      `db:"student_id"`
	StartedAt         time.Time  `db:"started_at"`
	EndedAt           *time.Time `db:"ended_at"`
	Status            string     `db:"status"`
	CycleCount        int        `db:"cycle_count"`
	CurrentGrade      string     `db:"current_grade"`
	Grade             string     `db:"grade"` // target grade
	StudentRequest    string     `db:"student_request"`
	SelfAssessment    string     `db:"self_assessment"`
	WeakTopics        string     `db:"weak_topics"` // comma-separated question_bank.topic values
	CurrentQuestionID *int64     `db:"current_question_id"`
}

// WeakZone is a topic the interviewer suspects (or has confirmed) the
// student struggles with, tracked across sessions. It is unique per
// (student_id, zone_text) so repeated observations update the same row.
type WeakZone struct {
	ID        int64     `db:"id"`
	StudentID int64     `db:"student_id"`
	ZoneText  string    `db:"zone_text"`
	Status    string    `db:"status"` // "hypothesis" | "confirmed"
	UpdatedAt time.Time `db:"updated_at"`
}

// QuestionBank is a reusable interview question.
//
// Grade holds one of: "джун", "джун-мидл", "мидл", "мидл-сеньор", "сеньор".
// Topic holds one of: "интеграции", "архитектура", "бд", "требования",
// "безопасность", "подача".
type QuestionBank struct {
	ID           int64  `db:"id"`
	QuestionText string `db:"question_text"`
	Grade        string `db:"grade"`
	Topic        string `db:"topic"`
	Followup1    string `db:"followup_1"`
	Followup2    string `db:"followup_2"`
	AnswerJunior string `db:"answer_junior"`
	AnswerMiddle string `db:"answer_middle"`
	AnswerSenior string `db:"answer_senior"`
	Source       string `db:"source"`
}

// SessionSummary is the LLM-generated wrap-up of a finished session.
type SessionSummary struct {
	ID          int64     `db:"id"`
	SessionID   int64     `db:"session_id"`
	SummaryText string    `db:"summary_text"`
	CreatedAt   time.Time `db:"created_at"`
}

// StudentReport is a per-student activity summary aggregated across
// students, sessions and weak_zones for the admin /report command. It
// is a read model, not a table row.
type StudentReport struct {
	TelegramID        int64
	Name              string
	CompletedSessions int
	LastSessionAt     *time.Time // nil if the student never started a session
	WeakZones         []WeakZone
}
