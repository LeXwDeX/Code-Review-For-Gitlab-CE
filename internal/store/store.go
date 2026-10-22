// Package store defines persistence models and sentinel errors shared by the
// SQLite implementation and its callers.
package store

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrNotFound  = errors.New("store: not found")
	ErrConflict  = errors.New("store: conflict")
	ErrLeaseLost = errors.New("store: lease lost")
)

const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobDone      = "done"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

const (
	DeliveryEnqueued = "enqueued"
	DeliveryIgnored  = "ignored"
	DeliveryRejected = "rejected"
)

// Project matches the projects table. TokenEnc and WebhookSecretEnc only
// contain ciphertext and are excluded from JSON serialization.
type Project struct {
	ID                int64           `json:"id"`
	PathWithNamespace string          `json:"path_with_namespace"`
	WebURL            string          `json:"web_url"`
	DefaultBranch     string          `json:"default_branch"`
	TokenEnc          []byte          `json:"-"`
	TokenExpiresAt    *time.Time      `json:"token_expires_at,omitempty"`
	TokenScopes       []string        `json:"token_scopes"`
	WebhookSecretEnc  []byte          `json:"-"`
	HookID            *int64          `json:"hook_id,omitempty"`
	Profile           string          `json:"profile"`
	Settings          json.RawMessage `json:"settings"`
	Enabled           bool            `json:"enabled"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// Job is the persistent queue record. State is managed by the store.
type Job struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Key         string          `json:"key,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	State       string          `json:"state"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_attempts"`
	RunAfter    time.Time       `json:"run_after"`
	LeaseOwner  string          `json:"lease_owner,omitempty"`
	LeaseUntil  *time.Time      `json:"lease_until,omitempty"`
	LastError   string          `json:"last_error,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// Delivery is a deduplicated webhook delivery record. It deliberately has no
// field for the raw webhook body or secret.
type Delivery struct {
	UUID       string    `json:"uuid"`
	ProjectID  int64     `json:"project_id"`
	Event      string    `json:"event"`
	Outcome    string    `json:"outcome"`
	Reason     string    `json:"reason,omitempty"`
	JobID      string    `json:"job_id,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
}
