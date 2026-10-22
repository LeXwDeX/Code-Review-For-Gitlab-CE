// Package store defines persistence records shared by the SQLite implementation
// and its callers.
package store

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	// ErrInvalidCursor means an opaque list cursor could not be decoded.
	ErrInvalidCursor = errors.New("store: invalid cursor")
	// ErrContextLocked means a generated refresh tried to overwrite a locked
	// context document. Manual edits use the explicit manual save method.
	ErrContextLocked = errors.New("store: context document is locked")
)

// LLMCall is one audited provider attempt. Request and Response are
// decompressed JSON only when IncludePayload was requested.
type LLMCall struct {
	ID           string          `json:"id"`
	WorkflowID   string          `json:"workflow_id"`
	NodeID       string          `json:"node_id"`
	Turn         int             `json:"turn"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	Request      json.RawMessage `json:"request,omitempty"`
	Response     json.RawMessage `json:"response,omitempty"`
	InputTokens  int64           `json:"input_tokens"`
	OutputTokens int64           `json:"output_tokens"`
	CacheRead    int64           `json:"cache_read"`
	CacheWrite   int64           `json:"cache_write"`
	LatencyMS    int64           `json:"latency_ms"`
	StopReason   string          `json:"stop_reason,omitempty"`
	ErrorKind    string          `json:"error_kind,omitempty"`
	RequestID    string          `json:"request_id,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// LLMCallFilter narrows ListLLMCalls.
type LLMCallFilter struct {
	WorkflowID     string
	NodeID         string
	Cursor         string
	Limit          int
	IncludePayload bool
}

// Workspace is the project workspace state row.
type Workspace struct {
	ProjectID     int64      `json:"project_id"`
	State         string     `json:"state"`
	BaseSHA       string     `json:"base_sha,omitempty"`
	IndexSHA      string     `json:"index_sha,omitempty"`
	ContextSHA    string     `json:"context_sha,omitempty"`
	LastRefreshAt *time.Time `json:"last_refresh_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// ContextDoc is one generated or manually maintained context document.
// Active and Stale are computed from the workspace row, not stored.
type ContextDoc struct {
	ID         string    `json:"id"`
	ProjectID  int64     `json:"project_id"`
	BaseSHA    string    `json:"base_sha"`
	Kind       string    `json:"kind"`
	Source     string    `json:"source"`
	Content    string    `json:"content"`
	Locked     bool      `json:"locked"`
	Model      string    `json:"model,omitempty"`
	WorkflowID string    `json:"workflow_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedBy  *int64    `json:"updated_by,omitempty"`
	Active     bool      `json:"active"`
	Stale      bool      `json:"stale"`
}

// ContextDocFilter narrows ListContextDocs.
type ContextDocFilter struct {
	ProjectID int64
	Kind      string
	Cursor    string
	Limit     int
}

// Feedback is one GitLab feedback signal. A nil user is anonymous and must
// still be unique per publication and signal.
type Feedback struct {
	ID            string    `json:"id"`
	ProjectID     int64     `json:"project_id"`
	MRIID         int64     `json:"mr_iid"`
	PublicationID string    `json:"publication_id"`
	FindingID     string    `json:"finding_id,omitempty"`
	Signal        string    `json:"signal"`
	GitlabUserID  *int64    `json:"gitlab_user_id,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
}

// FeedbackFilter narrows ListFeedback and FeedbackTotals.
type FeedbackFilter struct {
	ProjectID     *int64
	MRIID         *int64
	PublicationID string
	Signal        string
	Cursor        string
	Limit         int
}

// FeedbackTotals aggregates feedback signals.
type FeedbackTotals struct {
	Total    int64            `json:"total"`
	BySignal map[string]int64 `json:"by_signal"`
}

// StatsFilter bounds workflow and audit aggregates. To is exclusive.
type StatsFilter struct {
	From      time.Time
	To        time.Time
	ProjectID *int64
}

// ProjectStats is the per-project part of Stats.
type ProjectStats struct {
	ProjectID  int64 `json:"project_id"`
	Runs       int64 `json:"runs"`
	Blocking   int64 `json:"blocking"`
	Published  int64 `json:"published"`
	CostMicros int64 `json:"cost_micros"`
}

// ModelStats is the per-provider/model part of Stats.
type ModelStats struct {
	Provider     string   `json:"provider"`
	Model        string   `json:"model"`
	Calls        int64    `json:"calls"`
	Errors       int64    `json:"errors"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	CostMicros   int64    `json:"cost_micros"`
	AvgLatencyMS *float64 `json:"avg_latency_ms"`
	P50LatencyMS *float64 `json:"p50_latency_ms"`
	ErrorRate    *float64 `json:"error_rate"`
}

// Stats is the M6 aggregate view. Ratios and durations stay nil when there is
// no denominator instead of pretending to be zero.
type Stats struct {
	Runs            int64            `json:"runs"`
	Blocking        int64            `json:"blocking"`
	BlockingRatio   *float64         `json:"blocking_ratio"`
	Published       int64            `json:"published"`
	OverturnedRatio *float64         `json:"overturned_ratio"`
	AvgDurationMS   *float64         `json:"avg_duration_ms"`
	P95DurationMS   *float64         `json:"p95_duration_ms"`
	CostMicros      int64            `json:"cost_micros"`
	ByProject       []ProjectStats   `json:"by_project"`
	ByModel         []ModelStats     `json:"by_model"`
	Decisions       map[string]int64 `json:"decisions"`
}
