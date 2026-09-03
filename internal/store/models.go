package store

import "time"

// jsonbSlice guarantees a non-nil slice so a jsonb column gets `[]` rather than
// SQL NULL when there are no entries.
func jsonbSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// nonNil guarantees a non-nil slice for a text[] column (empty array, not NULL).
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Member mirrors a row from TinkerAccess's roster.
type Member struct {
	ID                 int64
	Name               string // may be empty if TinkerAccess had it null
	Code               string // RFID badge code; may be empty
	Status             string // "A", "I", or "S"
	Active             bool
	FirstSeenAt        time.Time
	LastSyncedAt       time.Time
	SourceMissingSince *time.Time
}

// SlackIdentity links a normalized Slack display name to a member.
type SlackIdentity struct {
	ID                  int64
	MemberID            int64
	SlackNameNormalized string
	AddedBy             string
	AddedAt             time.Time
}

// Printer is one Pi/printer pair.
type Printer struct {
	ID                int64
	Slug              string
	DisplayName       string
	Model             string
	AllowedExtensions []string
	SafetyChecklist   []string
	SlackWebhookURL   string
	APIKeyHash        string
	DeviceID          string // "" for a hand-made printer
	Approved          bool
	EnrolledAt        *time.Time
	LastSeenAt        *time.Time
	CreatedAt         time.Time

	// Fleet self-update bookkeeping.
	AgentVersion        string     // last version the Pi reported
	AgentVersionAt      *time.Time // when AgentVersion last changed
	AgentTargetOverride string     // non-empty: pin this Pi to this version now
	AgentUpdateHold     bool       // true: never auto-update this Pi
}

// Certification is a member's resin-printer certification.
type Certification struct {
	ID          int64
	MemberID    int64
	PrinterID   int64
	CertifiedBy string
	CertifiedAt time.Time
	RevokedAt   *time.Time
}

// PrintJob tracks one physical print run.
type PrintJob struct {
	ID                  int64
	PrinterID           int64
	MemberID            *int64
	SlackNameUsed       string
	Filename            string
	SlicedForModel      string
	ChecklistAnswers    []string
	StartedAt           time.Time
	EstimatedSeconds    *int32
	ETAExact            bool
	EstimatedCompleteAt *time.Time
	EndedAt             *time.Time
	Status              string
	EndReason           *string
}

// DecisionLogEntry records one check/upload attempt.
type DecisionLogEntry struct {
	ID            int64
	PrinterID     *int64
	SlackNameUsed string
	MemberID      *int64
	Filename      string
	TS            time.Time
	Outcome       string
	Reason        string
}

// Admin is a central admin-UI login.
type Admin struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}
