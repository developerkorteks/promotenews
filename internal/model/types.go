package model

import "time"

// Account status constants for lifecycle tracking.
const (
	StatusInactive  = "inactive"
	StatusPairing   = "pairing"
	StatusOnline    = "online"
	StatusLoggedOut = "logged_out"
	StatusReplaced  = "replaced"
	StatusError     = "error"
)

// Account represents a WhatsApp device/account managed by the system.
type Account struct {
	ID         string    `json:"id" db:"id"`
	Label      string    `json:"label" db:"label"`
	Msisdn     string    `json:"msisdn" db:"msisdn"`
	Enabled    bool      `json:"enabled" db:"enabled"`
	DailyLimit int       `json:"daily_limit" db:"daily_limit"`
	Status     string    `json:"status" db:"status"`
	LastError  string    `json:"last_error,omitempty" db:"last_error"`
	CreatedAt  time.Time `json:"created_at" db:"created_at"`
	UpdatedAt  time.Time `json:"updated_at" db:"updated_at"`
}

// Group represents a WhatsApp group (chat) discovered via scanning for an account.
type Group struct {
	ID         string     `json:"id" db:"id"` // JID as string
	AccountID  string     `json:"account_id" db:"account_id"`
	Name       string     `json:"name" db:"name"`
	Enabled    bool       `json:"enabled" db:"enabled"`
	LastSentAt *time.Time `json:"last_sent_at,omitempty" db:"last_sent_at"`
	RiskScore  int        `json:"risk_score" db:"risk_score"`
	CreatedAt  time.Time  `json:"created_at" db:"created_at"`
}

// Campaign defines flexible promotional content (text + media).
type Campaign struct {
	ID                string    `json:"id" db:"id"`
	Name              string    `json:"name" db:"name"`
	Text              string    `json:"text" db:"text"`
	MediaImagesJSON   string    `json:"media_images_json" db:"media_images"`
	MediaVideosJSON   string    `json:"media_videos_json" db:"media_videos"`
	MediaStickersJSON string    `json:"media_stickers_json" db:"media_stickers"`
	MediaDocsJSON     string    `json:"media_docs_json" db:"media_docs"`
	Enabled           bool      `json:"enabled" db:"enabled"`
	PerGroupVariants  int       `json:"per_group_variants" db:"per_group_variants"`
	CreatedAt         time.Time `json:"created_at" db:"created_at"`
	UpdatedAt         time.Time `json:"updated_at" db:"updated_at"`
}

// Schedule configures anti-spam safe scheduling for a campaign/account.
type Schedule struct {
	ID          string    `json:"id" db:"id"`
	CampaignID  string    `json:"campaign_id" db:"campaign_id"`
	AccountID   string    `json:"account_id" db:"account_id"`
	BatchSize   int       `json:"batch_size" db:"batch_size"`
	StartHour   int       `json:"start_hour" db:"start_hour"`
	EndHour     int       `json:"end_hour" db:"end_hour"`
	MinDelaySec int       `json:"min_delay_sec" db:"min_delay_sec"`
	MaxDelaySec int       `json:"max_delay_sec" db:"max_delay_sec"`
	DaysMask    string    `json:"days_mask" db:"days_mask"` // e.g. "Mon,Tue,Wed,Thu,Fri,Sat,Sun" or "Mon-Fri"
	DailyLimit  int       `json:"daily_limit" db:"daily_limit"`
	Enabled     bool      `json:"enabled" db:"enabled"`
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time `json:"updated_at" db:"updated_at"`
}

// LogEntry keeps audit/log for send attempts for monitoring & pause triggers.
type LogEntry struct {
	ID           int       `json:"id" db:"id"`
	TS           time.Time `json:"ts" db:"ts"`
	AccountID    string    `json:"account_id" db:"account_id"`
	GroupID      string    `json:"group_id" db:"group_id"`
	CampaignID   string    `json:"campaign_id" db:"campaign_id"`
	Status       string    `json:"status" db:"status"` // sent|failed|paused|skipped
	Error        string    `json:"error" db:"error"`
	MessagePrev  string    `json:"message_preview" db:"message_preview"`
	Attempt      int       `json:"attempt" db:"attempt"`
	ScheduledFor time.Time `json:"scheduled_for" db:"scheduled_for"`
}
