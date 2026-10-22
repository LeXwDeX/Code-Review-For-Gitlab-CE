package store

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrSuperseded means a review workflow request was built for a generation or
// head that is no longer current. It is distinct from ErrConflict so callers
// can treat "a newer push won" as an expected outcome.
var ErrSuperseded = errors.New("store: review superseded")

// ReviewControl is the durable per-MR review intent. Generation increments on
// every accepted review.run or review.cancel enqueue, including a replacement
// of a queued job. A duplicate delivery never increments it.
type ReviewControl struct {
	ProjectID  int64     `json:"project_id"`
	MRIID      int64     `json:"mr_iid"`
	Generation int64     `json:"generation"`
	HeadSHA    string    `json:"head_sha,omitempty"`
	Cancelled  bool      `json:"cancelled"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ReviewCommand is one authenticated review.run or review.cancel enqueue.
// The store derives the job kind and key, and assigns the generation inside
// the same transaction that writes the job. Payload must be a JSON object with
// job-specific fields (for example trigger, profile, full); it must not carry
// a generation because the store owns that field. ProjectID, MRIID and HeadSHA
// are always written by the store and override identical payload keys.
//
// HeadSHA is required for run and must be empty for cancel. Cancel preserves
// the previously recorded head so publication can still detect a stale MR.
type ReviewCommand struct {
	ProjectID   int64
	MRIID       int64
	HeadSHA     string
	Cancel      bool
	Payload     json.RawMessage
	MaxAttempts int
}

// ReviewEnqueueResult reports the durable outcome of an ingress call.
// Generation is zero when no review command was written (ignored delivery).
type ReviewEnqueueResult struct {
	JobID      string
	Duplicate  bool
	Generation int64
}

const (
	WorkflowPublicationPending   = "pending"
	WorkflowPublicationPublished = "published"
	WorkflowPublicationSkipped   = "skipped"
)

// WorkflowDossier is the sanitized input snapshot needed to resume or publish
// a workflow. Data is decompressed JSON on read and compressed at rest.
//
// The store validates bounded size and JSON syntax only. It cannot recognize
// every credential, so the caller must sanitize Data (no project tokens,
// provider keys, MCP secrets, signed URLs, or absolute temporary paths) before
// calling SaveWorkflowDossier. Events and workflow metadata should carry only
// small hashes/pointers, not a second copy of this payload.
type WorkflowDossier struct {
	WorkflowID       string          `json:"workflow_id"`
	JobID            string          `json:"job_id,omitempty"`
	Generation       int64           `json:"generation"`
	Version          int             `json:"version"`
	Hash             string          `json:"hash"`
	Data             json.RawMessage `json:"data"`
	PublicationState string          `json:"publication_state"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// PendingPublication is one completed workflow whose dossier has not been
// published yet. It deliberately omits the dossier body.
type PendingPublication struct {
	WorkflowID string    `json:"workflow_id"`
	JobID      string    `json:"job_id,omitempty"`
	ProjectID  int64     `json:"project_id"`
	MRIID      int64     `json:"mr_iid"`
	HeadSHA    string    `json:"head_sha,omitempty"`
	Generation int64     `json:"generation"`
	UpdatedAt  time.Time `json:"updated_at"`
}
