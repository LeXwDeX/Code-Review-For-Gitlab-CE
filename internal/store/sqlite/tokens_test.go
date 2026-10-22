package sqlite

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func TestFindAPITokenByHash(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	owner := store.TokenUser{ID: 7, Username: "alice", Name: "Alice", Role: "admin"}
	plaintext := []byte("crc_test-token")
	sum := sha256.Sum256(plaintext)
	expires := clock.Now().Add(time.Hour)
	if _, err := s.CreateAPIToken(ctx, owner, store.APIToken{
		Name: "ci", TokenHash: sum[:], ExpiresAt: &expires,
	}); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	token, user, err := s.FindAPITokenByHash(ctx, sum[:], clock.Now())
	if err != nil {
		t.Fatalf("FindAPITokenByHash: %v", err)
	}
	if token.Name != "ci" || token.LastUsedAt == nil || !token.LastUsedAt.Equal(clock.Now()) {
		t.Fatalf("token = %+v", token)
	}
	if user == nil || user.ID != 7 || user.Username != "alice" || user.Role != "admin" {
		t.Fatalf("user = %+v", user)
	}

	unknown := sha256.Sum256([]byte("crc_other"))
	if _, _, err := s.FindAPITokenByHash(ctx, unknown[:], clock.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown token = %v", err)
	}

	clock.Advance(2 * time.Hour)
	if _, _, err := s.FindAPITokenByHash(ctx, sum[:], clock.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired token = %v", err)
	}
	tokens, err := s.ListAPITokens(ctx, 7)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("ListAPITokens = %+v, %v", tokens, err)
	}
	if tokens[0].LastUsedAt == nil || !tokens[0].LastUsedAt.Equal(expires.Add(-time.Hour)) {
		t.Fatalf("expired lookup changed last_used_at: %+v", tokens[0].LastUsedAt)
	}
	if _, _, err := s.FindAPITokenByHash(ctx, []byte{1, 2}, clock.Now()); err == nil {
		t.Fatal("short hash accepted")
	}
}
