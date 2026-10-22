package gitlab

import "time"

// FileDiff is one changed file from the merge request diff endpoint.
type FileDiff struct {
	OldPath       string
	NewPath       string
	Diff          string
	NewFile       bool
	RenamedFile   bool
	DeletedFile   bool
	GeneratedFile bool
	Collapsed     bool
	TooLarge      bool
}

// Version is a merge request diff version. The three SHAs are required for
// inline discussion positions.
type Version struct {
	ID       int64
	BaseSHA  string
	StartSHA string
	HeadSHA  string
}

// Pipeline is the pipeline projection used in the review dossier.
type Pipeline struct {
	ID     int64
	IID    int64
	Status string
	Ref    string
	SHA    string
	WebURL string
}

// Job is a CI job belonging to a pipeline.
type Job struct {
	ID            int64
	Name          string
	Stage         string
	Status        string
	AllowFailure  bool
	FailureReason string
	WebURL        string
}

// Issue is an issue that closes when the merge request is merged.
type Issue struct {
	IID         int64
	Title       string
	State       string
	Description string
	Labels      []string
	WebURL      string
}

// Note is a merge request note (or discussion note).
type Note struct {
	ID             int64
	Body           string
	AuthorID       int64
	AuthorUsername string
	System         bool
	Resolvable     bool
	Resolved       bool
	Position       *Position
	CreatedAt      *time.Time
}

// Discussion is a merge request discussion thread.
type Discussion struct {
	ID             string
	IndividualNote bool
	Notes          []Note
}

// Award is an emoji reaction on a note.
type Award struct {
	ID       int64
	Name     string
	UserID   int64
	Username string
}

// CommitStatus is the payload written for a commit status.
type CommitStatus struct {
	State       string
	Name        string
	TargetURL   string
	Description string
}

// NewDiscussion is a body plus an optional inline position.
type NewDiscussion struct {
	Body     string
	Position *Position
}

// Side selects which side of a diff a line number refers to.
type Side string

const (
	SideNew Side = "new"
	SideOld Side = "old"
)

// Position is the GitLab discussion position payload.
type Position struct {
	BaseSHA  string
	StartSHA string
	HeadSHA  string
	OldPath  string
	NewPath  string
	OldLine  int
	NewLine  int
}
