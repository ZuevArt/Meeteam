package model

import "time"

const (
	HackathonsRusSourceID  = "hackathons-rus"
	CodenrockSourceID      = "codenrock"
	HackathonsProSourceID  = "hackathons-pro"
	HackathonRadarSourceID = "hackathon-radar"
	KaggleSourceID         = "kaggle"
	HackListSourceID       = "hacklist"
	DevpostSourceID        = "devpost"
	DevfolioSourceID       = "devfolio"
)

type Event struct {
	ID                     string     `json:"id"`
	SourceID               string     `json:"source_id"`
	SourceName             string     `json:"source_name"`
	SourceEventID          string     `json:"source_event_id"`
	Title                  string     `json:"title"`
	Description            string     `json:"description,omitempty"`
	StartAt                *time.Time `json:"start_at,omitempty"`
	EndAt                  *time.Time `json:"end_at,omitempty"`
	StartPrecision         string     `json:"start_precision"`
	RegistrationDeadlineAt *time.Time `json:"registration_deadline_at,omitempty"`
	ListingExpiresAt       *time.Time `json:"listing_expires_at,omitempty"`
	Format                 string     `json:"format,omitempty"`
	Organizer              string     `json:"organizer,omitempty"`
	OfficialURL            string     `json:"official_url"`
	SourceURL              string     `json:"source_url"`
	ImageURL               string     `json:"image_url,omitempty"`
	Tags                   []string   `json:"tags,omitempty"`
	ParsingNotes           []string   `json:"parsing_notes,omitempty"`
	RawFingerprint         string     `json:"raw_fingerprint"`
	FirstSeenAt            time.Time  `json:"first_seen_at"`
	LastSeenAt             time.Time  `json:"last_seen_at"`
}

type RejectedEvent struct {
	SourceEventID string `json:"source_event_id"`
	Title         string `json:"title,omitempty"`
	Reason        string `json:"reason"`
}

type ImportResult struct {
	SourceID     string          `json:"source_id"`
	StartedAt    time.Time       `json:"started_at"`
	FinishedAt   time.Time       `json:"finished_at"`
	FetchedCount int             `json:"fetched_count"`
	ValidCount   int             `json:"valid_count"`
	Events       []Event         `json:"events"`
	Rejected     []RejectedEvent `json:"rejected"`
	Warnings     []string        `json:"warnings,omitempty"`
}

type ImportRun struct {
	ID            string          `json:"id"`
	SourceID      string          `json:"source_id"`
	StartedAt     time.Time       `json:"started_at"`
	FinishedAt    time.Time       `json:"finished_at"`
	Status        string          `json:"status"`
	FetchedCount  int             `json:"fetched_count"`
	ValidCount    int             `json:"valid_count"`
	UpsertedCount int             `json:"upserted_count"`
	RejectedCount int             `json:"rejected_count"`
	ErrorCode     string          `json:"error_code,omitempty"`
	ErrorSummary  string          `json:"error_summary,omitempty"`
	Rejected      []RejectedEvent `json:"rejected,omitempty"`
}

type SourceState struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	BaseURL        string     `json:"base_url"`
	ImportOrder    int        `json:"import_order"`
	Enabled        bool       `json:"enabled"`
	Status         string     `json:"status"`
	AdapterVersion string     `json:"adapter_version"`
	LastAttemptAt  *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt  *time.Time `json:"last_success_at,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	FetchedCount   int        `json:"fetched_count"`
	ValidCount     int        `json:"valid_count"`
	RejectedCount  int        `json:"rejected_count"`
}

type Snapshot struct {
	Events  []Event       `json:"events"`
	Sources []SourceState `json:"sources"`
	Runs    []ImportRun   `json:"runs"`
}
