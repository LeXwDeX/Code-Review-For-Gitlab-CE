package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testMasterKey    = "0123456789abcdef0123456789abcdef"
	testBootstrapKey = "abcdefghijklmnopqrstuvwxyz012345"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("REVIEW_MASTER_KEY", base64.StdEncoding.EncodeToString([]byte(testMasterKey)))
	t.Setenv("REVIEW_ADMIN_TOKEN", testBootstrapKey)
}

func writeConfig(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func unsetEnv(t *testing.T, name string) {
	t.Helper()
	old, had := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(name, old)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

func loadConfig(t *testing.T, paths ...string) *Config {
	t.Helper()
	cfg, err := Load(paths...)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	return cfg
}

func requireErrorContains(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", wants)
	}
	text := err.Error()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("error %q does not contain %q", text, want)
		}
	}
}

func requireErrorNotContains(t *testing.T, err error, unwanted ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	text := err.Error()
	for _, value := range unwanted {
		if strings.Contains(text, value) {
			t.Errorf("error %q leaked %q", text, value)
		}
	}
}
