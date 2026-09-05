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
	WeakZoneStatusClosed     = "closed"
)

const (
	QuestionAttemptWaitingPrimary        = "WAITING_PRIMARY"
	QuestionAttemptPrimaryFeedbackReady  = "PRIMARY_FEEDBACK_READY"
	QuestionAttemptWaitingFollowup       = "WAITING_FOLLOWUP"
	QuestionAttemptFollowupFeedbackReady = "FOLLOWUP_FEEDBACK_READY"
	QuestionAttemptWaitingFollowup2      = "WAITING_FOLLOWUP_2"
	QuestionAttemptFinalFeedbackReady    = "FINAL_FEEDBACK_READY"
	QuestionAttemptWaitingVector         = "WAITING_VECTOR"
	QuestionAttemptCompleted             = "COMPLETED"
)

// Qualification steps. The value is the answer the bot is currently waiting
// for; QualificationStepDone means all four answers are stored.
const (
	QualificationStepCurrentGrade = iota
	QualificationStepTargetGrade
	QualificationStepStrongZones
	QualificationStepWeakZones
	QualificationStepDone
)

// Session status values. The first five are the interview FSM phases,
// walked in order by internal/handlers; the last two are terminal
// states set by EndSession once a session stops being active.
const (
	SessionStatusInstruction         = "INSTRUCTION"
	SessionStatusQualification       = "QUALIFICATION"
	SessionStatusProfileConfirmation = "PROFILE_CONFIRMATION"
	SessionStatusKDIRLesson          = "KDIR_LESSON"
	SessionStatusAudit               = "AUDIT"
	SessionStatusQuestionCycle       = "QUESTION_CYCLE"
	SessionStatusSummary             = "SUMMARY"
	SessionStatusCompleted           = "COMPLETED"
	SessionStatusAbandoned           = "ABANDONED"
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
// CurrentGrade, Grade (target grade), StrongZones and WeakZonesInput are the
// four fields used by the deterministic QUALIFICATION flow.
// QualificationStep records which answer the bot expects next. Direction,
// Experience, InterviewTarget, StudentRequest and SelfAssessment are retained
// for schema compatibility with sessions created by older versions. Grade is
// used by PickQuestion for question difficulty throughout the rest of the
// session.
// WeakTopics is a comma-separated list of question_bank.topic values mapped
// from the candidate's confirmed weak-zone answer and used as a priority
// filter in QUESTION_CYCLE.
// CycleCount counts fully evaluated three-answer blocks. SESSION_CYCLE_LIMIT
// controls how often the bot offers a continue-or-finish checkpoint.
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
	Direction         string     `db:"direction"`
	Experience        string     `db:"experience"`
	InterviewTarget   string     `db:"interview_target"`
	QualificationStep int        `db:"qualification_step"`
	NextTopic         string     `db:"next_topic"`
	StrongZones       string     `db:"strong_zones"`
	WeakZonesInput    string     `db:"weak_zones_input"`
}

// QuestionAttempt persists one complete three-answer interview block.
type QuestionAttempt struct {
	ID                int64     `db:"id"`
	SessionID         int64     `db:"session_id"`
	QuestionID        int64     `db:"question_id"`
	PrimaryAnswer     string    `db:"primary_answer"`
	PrimaryFeedback   string    `db:"primary_feedback"`
	FollowupQuestion  string    `db:"followup_question"`
	FollowupAnswer    string    `db:"followup_answer"`
	FollowupFeedback  string    `db:"followup_feedback"`
	Followup2Question string    `db:"followup_2_question"`
	Followup2Answer   string    `db:"followup_2_answer"`
	Followup2Feedback string    `db:"followup_2_feedback"`
	FinalFeedback     string    `db:"final_feedback"`
	SelectedVector    string    `db:"selected_vector"`
	Status            string    `db:"status"`
	CreatedAt         time.Time `db:"created_at"`
	UpdatedAt         time.Time `db:"updated_at"`
}

// QuestionAttemptReport joins an attempt with the bank question for the final
// report context.
type QuestionAttemptReport struct {
	QuestionText      string
	Topic             string
	PrimaryAnswer     string
	FollowupQuestion  string
	FollowupAnswer    string
	Followup2Question string
	Followup2Answer   string
	FinalFeedback     string
}

// WeakZone is a topic the interviewer suspects (or has confirmed) the
// student struggles with, tracked across sessions. It is unique per
// (student_id, zone_text) so repeated observations update the same row.
type WeakZone struct {
	ID        int64     `db:"id"`
	StudentID int64     `db:"student_id"`
	ZoneText  string    `db:"zone_text"`
	Status    string    `db:"status"` // "hypothesis" | "confirmed" | "closed"
	UpdatedAt time.Time `db:"updated_at"`
}

// QuestionBank is a reusable interview question.
//
// Grade holds one of: "джун", "джун-мидл", "мидл", "мидл-сеньор", "сеньор".
// Topic holds one of: "интеграции", "архитектура", "бд", "требования",
// "безопасность", "soft-skills".
type QuestionBank struct {
	ID               int64  `db:"id"`
	QuestionText     string `db:"question_text"`
	QuestionContext  string `db:"question_context"`
	Grade            string `db:"grade"`
	Topic            string `db:"topic"`
	Followup1        string `db:"followup_1"`
	Followup1Context string `db:"followup_1_context"`
	Followup2        string `db:"followup_2"`
	Followup2Context string `db:"followup_2_context"`
	AnswerJunior     string `db:"answer_junior"`
	AnswerMiddle     string `db:"answer_middle"`
	AnswerSenior     string `db:"answer_senior"`
	Source           string `db:"source"`
}

// SessionSummary is the LLM-generated wrap-up of a finished session.
type SessionSummary struct {
	ID          int64     `db:"id"`
	SessionID   int64     `db:"session_id"`
	SummaryText string    `db:"summary_text"`
	CreatedAt   time.Time `db:"created_at"`
}

// StudentReport is a per-student activity and LLM usage summary for the
// admin /report command. It is a read model, not a table row.
type StudentReport struct {
	TelegramID        int64
	Name              string
	AccessCode        string
	CompletedSessions int
	LastSessionAt     *time.Time // nil if the student never started a session
	Calls             int64
	PromptTokens      int64
	CompletionTokens  int64
	TotalTokens       int64
	CachedTokens      int64
}

// TokenUsageReport is the admin-facing aggregate for one access code. Codes
// that have not been used yet have no student identity and zero usage.
type TokenUsageReport struct {
	Code             string
	IsUsed           bool
	StudentID        *int64
	StudentName      string
	Calls            int64
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CachedTokens     int64
}
