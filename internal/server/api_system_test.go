package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func systemPlatform() *config.Config {
	return &config.Config{
		Providers: map[string]config.ProviderConfig{
			"claude": {
				Protocol: "anthropic", BaseURL: "https://api.anthropic.com",
				Model: "claude-sonnet", APIKeyEnv: "ANTHROPIC_API_KEY",
				MaxConcurrency: 4, RPM: 60,
			},
			"unused": {Protocol: "openai-chat", BaseURL: "https://llm.example.com", Model: "m"},
		},
		Profiles: map[string]config.ProfileConfig{"default": {}, "deep": {}},
		Secrets:  config.SecretsConfig{MasterKeyEnv: "REVIEW_MASTER_KEY"},
		Storage: config.StorageConfig{
			Driver: "sqlite",
			DSN:    "file:/srv/review.db?_pragma=password(hunter2)",
		},
		Auth: config.AuthConfig{
			BootstrapAdmins: []string{"alice"},
			OAuth:           config.OAuthConfig{ClientIDEnv: "GITLAB_CLIENT_ID", ClientSecretEnv: "GITLAB_CLIENT_SECRET"},
		},
		MCP:  config.MCPConfig{Config: "/etc/review/mcp.json"},
		Jobs: config.JobsConfig{Workers: 4},
		Blocks: map[string]config.BlockBinding{
			"review": {MCPTools: []string{"mcp__github__search"}, Skills: []string{"go-review"}},
		},
	}
}

func TestProvidersHideKeysAndReportStats(t *testing.T) {
	fake := newFakeReviewStore()
	fake.stats = store.Stats{
		ByModel: []store.ModelStats{
			{Provider: "claude", Model: "claude-sonnet", Calls: 10, Errors: 2, P50LatencyMS: ptr(120.0), ErrorRate: ptr(0.2)},
			{Provider: "claude", Model: "claude-haiku", Calls: 4, Errors: 1, P50LatencyMS: ptr(80.0), ErrorRate: ptr(0.25)},
		},
	}
	env := newM6Env(t, m6Options{store: fake, platform: systemPlatform()})
	client := env.loginAs("code-admin")

	response := client.request(http.MethodGet, "/api/v1/providers", nil, nil)
	assertStatus(t, response, http.StatusOK)
	text := string(response.body)
	for _, forbidden := range []string{"ANTHROPIC_API_KEY", "api_key_env", "secret"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("providers leaked %q: %s", forbidden, text)
		}
	}
	body := env.decode(t, response)
	providers := body["providers"].([]any)
	if len(providers) != 2 {
		t.Fatalf("providers = %v", providers)
	}
	first := providers[0].(map[string]any)
	if first["name"] != "claude" || first["protocol"] != "anthropic" {
		t.Fatalf("provider = %v", first)
	}
	stats := first["stats_24h"].(map[string]any)
	if stats["calls"] != float64(14) || stats["errors"] != float64(3) {
		t.Fatalf("stats_24h = %v", stats)
	}
	rate := stats["error_rate"].(float64)
	if rate < 0 || rate > 1 {
		t.Fatalf("error_rate outside [0,1]: %v", rate)
	}
	if rate != 3.0/14.0 {
		t.Fatalf("error_rate = %v", rate)
	}
	if stats["p50_latency_ms"] != float64(120) {
		t.Fatalf("p50 = %v", stats["p50_latency_ms"])
	}
	second := providers[1].(map[string]any)
	unusedStats := second["stats_24h"].(map[string]any)
	if unusedStats["calls"] != float64(0) || unusedStats["error_rate"] != nil || unusedStats["p50_latency_ms"] != nil {
		t.Fatalf("unused provider stats = %v", unusedStats)
	}

	// The 24h window ends at now and starts 24h earlier.
	if fake.lastStats.From.IsZero() || fake.lastStats.To.IsZero() {
		t.Fatalf("stats filter = %+v", fake.lastStats)
	}
	if fake.lastStats.To.Sub(fake.lastStats.From).Hours() != 24 {
		t.Fatalf("stats window = %v", fake.lastStats.To.Sub(fake.lastStats.From))
	}
}

func TestProviderTestRequiresOKBoolean(t *testing.T) {
	env := newM6Env(t, m6Options{platform: systemPlatform()})
	env.llm.result = ProviderTestResult{Model: "claude-sonnet", LatencyMS: 42}
	client := env.loginAs("code-admin")

	success := client.request(http.MethodPost, "/api/v1/providers/claude/test", nil, nil)
	assertStatus(t, success, http.StatusOK)
	body := env.decode(t, success)
	if body["ok"] != true || body["model"] != "claude-sonnet" || body["latency_ms"] != float64(42) {
		t.Fatalf("success body = %v", body)
	}
	if len(env.llm.names) != 1 || env.llm.names[0] != "claude" {
		t.Fatalf("tester names = %v", env.llm.names)
	}

	env.llm.err = ioError("dial tcp 10.0.0.1:443: connection refused")
	failure := client.request(http.MethodPost, "/api/v1/providers/claude/test", nil, nil)
	assertStatus(t, failure, http.StatusOK)
	failureBody := env.decode(t, failure)
	if failureBody["ok"] != false {
		t.Fatalf("failure body = %v", failureBody)
	}
	if failureBody["message"] == nil {
		t.Fatalf("failure has no message: %v", failureBody)
	}
	if strings.Contains(string(failure.body), "10.0.0.1") {
		t.Fatalf("failure leaked provider details: %s", failure.body)
	}

	unknown := client.request(http.MethodPost, "/api/v1/providers/missing/test", nil, nil)
	assertStatus(t, unknown, http.StatusNotFound)
}

type ioError string

func (e ioError) Error() string { return string(e) }

func TestStatsKeepsUndefinedValuesNull(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake, platform: systemPlatform()})
	client := env.loginAs("code-admin")

	response := client.request(http.MethodGet, "/api/v1/stats?from=2026-10-01T00:00:00Z&to=2026-10-21T00:00:00Z&project_id=42", nil, nil)
	assertStatus(t, response, http.StatusOK)
	var body map[string]any
	if err := json.Unmarshal(response.body, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, field := range []string{"blocking_ratio", "overturned_ratio", "avg_duration_ms", "p95_duration_ms"} {
		value, present := body[field]
		if !present || value != nil {
			t.Fatalf("%s = %v, want null", field, value)
		}
	}
	if body["by_project"] == nil || body["by_model"] == nil || body["decisions"] == nil {
		t.Fatalf("collections must be arrays/objects: %v", body)
	}
	if len(body["by_project"].([]any)) != 0 || len(body["by_model"].([]any)) != 0 {
		t.Fatalf("collections = %v", body)
	}
	if fake.lastStats.ProjectID == nil || *fake.lastStats.ProjectID != 42 {
		t.Fatalf("project filter = %+v", fake.lastStats)
	}

	invalid := client.request(http.MethodGet, "/api/v1/stats?from=2026-10-21T00:00:00Z&to=2026-10-01T00:00:00Z", nil, nil)
	assertStatus(t, invalid, http.StatusBadRequest)
}

func TestProfilesComeFromConfig(t *testing.T) {
	env := newM6Env(t, m6Options{platform: systemPlatform()})
	client := env.loginAs("code-viewer")

	response := client.request(http.MethodGet, "/api/v1/profiles", nil, nil)
	assertStatus(t, response, http.StatusOK)
	profiles := env.decode(t, response)["profiles"].([]any)
	if len(profiles) != 2 {
		t.Fatalf("profiles = %v", profiles)
	}
	first := profiles[0].(map[string]any)
	if first["name"] != "deep" || first["config"] == nil {
		t.Fatalf("profile = %v", first)
	}
}

func TestSettingsMaskSecrets(t *testing.T) {
	env := newM6Env(t, m6Options{platform: systemPlatform()})
	client := env.loginAs("code-viewer")

	response := client.request(http.MethodGet, "/api/v1/settings", nil, nil)
	assertStatus(t, response, http.StatusOK)
	text := string(response.body)
	for _, forbidden := range []string{
		"ANTHROPIC_API_KEY", "REVIEW_MASTER_KEY", "hunter2", "GITLAB_CLIENT_SECRET",
		"/srv/review.db", "file:",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("settings leaked %q: %s", forbidden, text)
		}
	}
	body := env.decode(t, response)
	providers := body["providers"].([]any)
	if len(providers) != 2 {
		t.Fatalf("providers = %v", providers)
	}
	if providers[0].(map[string]any)["api_key_configured"] != true {
		t.Fatalf("first provider = %v", providers[0])
	}
	if providers[1].(map[string]any)["api_key_configured"] != false {
		t.Fatalf("second provider = %v", providers[1])
	}
	secrets := body["secrets"].(map[string]any)
	if secrets["master_key_configured"] != true {
		t.Fatalf("secrets = %v", secrets)
	}
	auth := body["auth"].(map[string]any)
	if auth["oauth_configured"] != true {
		t.Fatalf("auth = %v", auth)
	}
	blocks := body["blocks"].(map[string]any)
	if blocks["review"] == nil {
		t.Fatalf("blocks = %v", blocks)
	}
}
