package store

import (
	"encoding/json"
	"time"
)

// ProjectFilter selects registered projects for the admin list endpoint. An
// empty query matches every project; a nil Enabled keeps both states.
type ProjectFilter struct {
	Query   string
	Enabled *bool
}

// ProjectUpdate replaces only the provided project fields.
type ProjectUpdate struct {
	Profile  *string
	Settings json.RawMessage
	Enabled  *bool
}

// ProjectTokenUpdate rotates the encrypted project access token together with
// its expiry and scopes.
type ProjectTokenUpdate struct {
	TokenEnc  []byte
	ExpiresAt *time.Time
	Scopes    []string
}
