package store

import "time"

// TokenUser is the authenticated session user that owns an API token. Users
// are upserted on first token creation because the users table is otherwise
// only populated from OAuth login.
type TokenUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Role     string `json:"role"`
}

// APIToken is one persisted API token. TokenHash is the SHA-256 digest of the
// plaintext token; the plaintext is never stored and is returned only once by
// the create endpoint.
type APIToken struct {
	ID         string     `json:"id"`
	UserID     int64      `json:"user_id"`
	Name       string     `json:"name"`
	TokenHash  []byte     `json:"-"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}
