package server

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func TestAPITokenCreateOnceAndRevoke(t *testing.T) {
	env := newM6Env(t, m6Options{})
	client := env.loginAs("code-admin")

	list := client.request(http.MethodGet, "/api/v1/api-tokens", nil, nil)
	assertStatus(t, list, http.StatusOK)
	if tokens := env.decode(t, list)["tokens"].([]any); len(tokens) != 0 {
		t.Fatalf("tokens = %v", tokens)
	}

	created := client.json(http.MethodPost, "/api/v1/api-tokens",
		map[string]any{"name": "ci token", "expires_in_days": 30})
	assertStatus(t, created, http.StatusCreated)
	body := env.decode(t, created)
	plaintext, _ := body["token"].(string)
	if !strings.HasPrefix(plaintext, "crc_") || len(plaintext) < 40 {
		t.Fatalf("plaintext = %q", plaintext)
	}
	if body["name"] != "ci token" {
		t.Fatalf("created = %v", body)
	}
	if body["expires_at"] == nil {
		t.Fatalf("missing expires_at: %v", body)
	}
	tokenID, _ := body["id"].(string)
	if tokenID == "" {
		t.Fatalf("missing id: %v", body)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, body["expires_at"].(string))
	if err != nil {
		t.Fatalf("expires_at parse: %v", err)
	}
	if expiresAt.Before(time.Now().Add(29 * 24 * time.Hour)) {
		t.Fatalf("expires_at = %v", expiresAt)
	}

	again := client.request(http.MethodGet, "/api/v1/api-tokens", nil, nil)
	assertStatus(t, again, http.StatusOK)
	againText := string(again.body)
	for _, secret := range []string{plaintext, "token_hash"} {
		if strings.Contains(againText, secret) {
			t.Fatalf("list leaked %q: %s", secret, againText)
		}
	}
	tokens := env.decode(t, again)["tokens"].([]any)
	if len(tokens) != 1 {
		t.Fatalf("tokens = %v", tokens)
	}
	entry := tokens[0].(map[string]any)
	if _, present := entry["token"]; present {
		t.Fatalf("list item includes plaintext: %v", entry)
	}
	if entry["id"] != tokenID {
		t.Fatalf("token id = %v", entry["id"])
	}

	// The store persists only the SHA-256 digest, and the owning users row was
	// created on first use (the token FK requires it).
	persisted, err := env.sqlite.ListAPITokens(context.Background(), 7)
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("persisted = %v", persisted)
	}
	sum := sha256.Sum256([]byte(plaintext))
	if len(persisted[0].TokenHash) != sha256.Size || string(persisted[0].TokenHash) != string(sum[:]) {
		t.Fatalf("stored hash mismatch")
	}

	// Token names may repeat; each create returns a fresh plaintext once.
	second := client.json(http.MethodPost, "/api/v1/api-tokens", map[string]any{"name": "ci token"})
	assertStatus(t, second, http.StatusCreated)
	secondBody := env.decode(t, second)
	if secondBody["token"] == plaintext || secondBody["id"] == tokenID {
		t.Fatalf("second token reused material: %v", secondBody)
	}
	if _, present := secondBody["expires_at"]; present {
		t.Fatalf("unexpected expiry: %v", secondBody)
	}

	revoked := client.request(http.MethodDelete, "/api/v1/api-tokens/"+tokenID, nil, nil)
	assertStatus(t, revoked, http.StatusNoContent)
	after := client.request(http.MethodGet, "/api/v1/api-tokens", nil, nil)
	assertStatus(t, after, http.StatusOK)
	if len(env.decode(t, after)["tokens"].([]any)) != 1 {
		t.Fatalf("after revoke = %s", after.body)
	}
	missing := client.request(http.MethodDelete, "/api/v1/api-tokens/"+tokenID, nil, nil)
	assertStatus(t, missing, http.StatusNotFound)

	// A viewer cannot reach token writes at all: the auth middleware rejects
	// unsafe methods before ownership is checked.
	viewer := env.loginAs("code-viewer")
	foreign := viewer.request(http.MethodDelete, "/api/v1/api-tokens/"+secondBody["id"].(string), nil, nil)
	assertStatus(t, foreign, http.StatusForbidden)
}

func TestAPITokenValidationAndSessionOnly(t *testing.T) {
	env := newM6Env(t, m6Options{})
	client := env.loginAs("code-admin")

	for _, body := range []map[string]any{
		{"name": ""},
		{"name": "x", "expires_in_days": -1},
		{"name": "x", "expires_in_days": 3651},
	} {
		response := client.json(http.MethodPost, "/api/v1/api-tokens", body)
		if response.status != http.StatusBadRequest {
			t.Fatalf("body %v status = %d", body, response.status)
		}
	}

	// The bootstrap bearer client has no user row; creation must not attach a
	// token to it.
	bearer := env.bearerClient()
	for _, response := range []apiResponse{
		bearer.request(http.MethodGet, "/api/v1/api-tokens", nil, nil),
		bearer.json(http.MethodPost, "/api/v1/api-tokens", map[string]any{"name": "x"}),
		bearer.request(http.MethodDelete, "/api/v1/api-tokens/abc", nil, nil),
	} {
		if response.status != http.StatusForbidden {
			t.Fatalf("bearer status = %d, body=%s", response.status, response.body)
		}
	}

	viewer := env.loginAs("code-viewer")
	// Viewers may list their own tokens but cannot create or revoke them.
	viewerList := viewer.request(http.MethodGet, "/api/v1/api-tokens", nil, nil)
	if viewerList.status != http.StatusOK {
		t.Fatalf("viewer list status = %d, body=%s", viewerList.status, viewerList.body)
	}
	if tokens := env.decode(t, viewerList)["tokens"].([]any); len(tokens) != 0 {
		t.Fatalf("viewer tokens = %v", tokens)
	}
	denied := viewer.json(http.MethodPost, "/api/v1/api-tokens", map[string]any{"name": "viewer"})
	if denied.status != http.StatusForbidden {
		t.Fatalf("viewer create status = %d, body=%s", denied.status, denied.body)
	}
}

func TestAPITokenAuthenticates(t *testing.T) {
	ctx := context.Background()
	env := newM6Env(t, m6Options{})
	session := env.loginAs("code-admin")
	created := session.json(http.MethodPost, "/api/v1/api-tokens", map[string]any{"name": "ci"})
	assertStatus(t, created, http.StatusCreated)
	body := env.decode(t, created)
	plaintext, _ := body["token"].(string)
	tokenID, _ := body["id"].(string)
	if plaintext == "" || tokenID == "" {
		t.Fatalf("created = %v", body)
	}

	client := &m6Client{t: t, base: env.server.URL, client: &http.Client{}, bearer: plaintext}
	me := client.request(http.MethodGet, "/api/v1/me", nil, nil)
	assertStatus(t, me, http.StatusOK)
	profile := env.decode(t, me)
	if profile["username"] != "alice" || profile["role"] != roleAdmin {
		t.Fatalf("me = %v", profile)
	}

	stored, err := env.sqlite.ListAPITokens(ctx, 7)
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	if len(stored) != 1 || stored[0].LastUsedAt == nil {
		t.Fatalf("last_used_at was not updated: %+v", stored)
	}

	// An admin API token may write without CSRF, exactly like other bearers.
	written := client.json(http.MethodPost, "/api/v1/projects", map[string]any{"project": "42", "token": "x"})
	assertStatus(t, written, http.StatusCreated)

	unknown := &m6Client{t: t, base: env.server.URL, client: &http.Client{}, bearer: "crc_unknown"}
	assertStatus(t, unknown.request(http.MethodGet, "/api/v1/me", nil, nil), http.StatusUnauthorized)

	revoked := session.request(http.MethodDelete, "/api/v1/api-tokens/"+tokenID, nil, nil)
	assertStatus(t, revoked, http.StatusNoContent)
	assertStatus(t, client.request(http.MethodGet, "/api/v1/me", nil, nil), http.StatusUnauthorized)
}

func TestAPITokenExpiryAndViewerRole(t *testing.T) {
	ctx := context.Background()
	env := newM6Env(t, m6Options{})

	expiredPlain := "crc_expired-token-value"
	sum := sha256.Sum256([]byte(expiredPlain))
	past := time.Now().Add(-time.Hour)
	if _, err := env.sqlite.CreateAPIToken(ctx, store.TokenUser{
		ID: 7, Username: "alice", Name: "Alice Admin", Role: roleAdmin,
	}, store.APIToken{Name: "expired", TokenHash: sum[:], ExpiresAt: &past}); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	expired := &m6Client{t: t, base: env.server.URL, client: &http.Client{}, bearer: expiredPlain}
	assertStatus(t, expired.request(http.MethodGet, "/api/v1/me", nil, nil), http.StatusUnauthorized)

	viewerPlain := "crc_viewer-token-value"
	viewerSum := sha256.Sum256([]byte(viewerPlain))
	if _, err := env.sqlite.CreateAPIToken(ctx, store.TokenUser{
		ID: 8, Username: "bob", Name: "Bob Viewer", Role: roleViewer,
	}, store.APIToken{Name: "viewer", TokenHash: viewerSum[:]}); err != nil {
		t.Fatalf("CreateAPIToken viewer: %v", err)
	}
	viewer := &m6Client{t: t, base: env.server.URL, client: &http.Client{}, bearer: viewerPlain}
	me := viewer.request(http.MethodGet, "/api/v1/me", nil, nil)
	assertStatus(t, me, http.StatusOK)
	if role := env.decode(t, me)["role"]; role != roleViewer {
		t.Fatalf("viewer role = %v", role)
	}
	write := viewer.json(http.MethodPost, "/api/v1/projects", map[string]any{"project": "42", "token": "x"})
	assertStatus(t, write, http.StatusForbidden)
}
