package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type capturedRequest struct {
	method string
	path   string
	auth   string
	body   map[string]any
}

func captureHandler(t *testing.T, status int, response string, captured *capturedRequest) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.auth = r.Header.Get("Authorization")
		if r.Body != nil {
			data, _ := io.ReadAll(r.Body)
			if len(data) > 0 {
				if err := json.Unmarshal(data, &captured.body); err != nil {
					t.Errorf("request body is not JSON: %s", data)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}
}

func runCtl(t *testing.T, args []string, env map[string]string, client *http.Client) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), args, func(name string) string { return env[name] }, &stdout, &stderr, client)
	return stdout.String(), stderr.String(), err
}

func TestRegisterCommand(t *testing.T) {
	var captured capturedRequest
	ts := httptest.NewServer(captureHandler(t, http.StatusCreated,
		`{"project":{"id":42},"job_id":"job-1","webhook_secret":"one-time-secret"}`, &captured))
	defer ts.Close()

	stdout, _, err := runCtl(t, []string{
		"--url", ts.URL,
		"--api-token-env", "ADMIN_TOKEN",
		"projects", "register",
		"--project", "group/repo",
		"--token-env", "PROJECT_TOKEN",
		"--profile", "deep",
		"--auto-webhook",
		"--settings", `{"skip_draft":false}`,
	}, map[string]string{
		"ADMIN_TOKEN":   "admin-secret-value",
		"PROJECT_TOKEN": "glpat-project-token",
	}, ts.Client())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if captured.method != http.MethodPost || captured.path != "/api/v1/projects" {
		t.Fatalf("request = %s %s", captured.method, captured.path)
	}
	if captured.auth != "Bearer admin-secret-value" {
		t.Fatalf("auth = %q", captured.auth)
	}
	wantBody := map[string]any{
		"project":      "group/repo",
		"token":        "glpat-project-token",
		"profile":      "deep",
		"auto_webhook": true,
		"settings":     map[string]any{"skip_draft": false},
	}
	for key, value := range wantBody {
		if captured.body[key] == nil {
			t.Errorf("missing body field %q", key)
			continue
		}
		if key == "settings" {
			settings, _ := captured.body[key].(map[string]any)
			if settings["skip_draft"] != false {
				t.Errorf("settings = %v", captured.body[key])
			}
			continue
		}
		if key != "auto_webhook" && captured.body[key] != value {
			t.Errorf("body[%s] = %v, want %v", key, captured.body[key], value)
		}
	}
	if captured.body["auto_webhook"] != true || captured.body["token"] != "glpat-project-token" {
		t.Errorf("body = %v", captured.body)
	}
	if !strings.Contains(stdout, "one-time-secret") {
		t.Fatalf("stdout = %q", stdout)
	}
	for _, forbidden := range []string{"admin-secret-value", "glpat-project-token"} {
		if strings.Contains(stdout, forbidden) {
			t.Fatalf("stdout leaked %q", forbidden)
		}
	}
}

func TestListAndGetCommands(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		var captured capturedRequest
		ts := httptest.NewServer(captureHandler(t, http.StatusOK, `{"projects":[{"id":42}]}`, &captured))
		defer ts.Close()
		stdout, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "list"}, map[string]string{
			"REVIEW_ADMIN_TOKEN": "admin",
		}, ts.Client())
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if captured.method != http.MethodGet || captured.path != "/api/v1/projects" {
			t.Fatalf("request = %s %s", captured.method, captured.path)
		}
		if !strings.Contains(stdout, `"id": 42`) {
			t.Fatalf("stdout = %s", stdout)
		}
	})
	t.Run("get", func(t *testing.T) {
		var captured capturedRequest
		ts := httptest.NewServer(captureHandler(t, http.StatusOK, `{"id":42}`, &captured))
		defer ts.Close()
		_, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "get", "42"}, map[string]string{
			"REVIEW_ADMIN_TOKEN": "admin",
		}, ts.Client())
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if captured.path != "/api/v1/projects/42" {
			t.Fatalf("path = %q", captured.path)
		}
	})
}

func TestStatusErrorsAreMeaningful(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"project not registered","request_id":"req-1"}}`)
	}))
	defer ts.Close()
	_, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "list"}, map[string]string{
		"REVIEW_ADMIN_TOKEN": "admin",
	}, ts.Client())
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"404", "not_found", "req-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestStatusErrorsDoNotEchoArbitraryBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `<html>upstream-secret-detail</html>`)
	}))
	defer ts.Close()
	_, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "list"}, map[string]string{
		"REVIEW_ADMIN_TOKEN": "admin",
	}, ts.Client())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "upstream-secret-detail") {
		t.Fatalf("error leaked body: %v", err)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()

	_, _, err := runCtl(t, []string{"--url", redirect.URL, "projects", "list"}, map[string]string{
		"REVIEW_ADMIN_TOKEN": "admin",
	}, redirect.Client())
	if err == nil {
		t.Fatal("expected redirect to be rejected")
	}
	if targetHits.Load() != 0 {
		t.Fatal("redirect target was called")
	}
}

func TestMissingEnvironment(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer ts.Close()
	_, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "list"}, map[string]string{}, ts.Client())
	if err == nil || !strings.Contains(err.Error(), "REVIEW_ADMIN_TOKEN") {
		t.Fatalf("error = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("request was sent without a token")
	}

	_, _, err = runCtl(t, []string{
		"--url", ts.URL, "projects", "register", "--project", "42", "--token-env", "PROJECT_TOKEN",
	}, map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, ts.Client())
	if err == nil || !strings.Contains(err.Error(), "PROJECT_TOKEN") {
		t.Fatalf("error = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("request was sent without a project token")
	}
}

func TestInvalidSettingsFailsBeforeRequest(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer ts.Close()
	for _, settings := range []string{`{`, `[]`} {
		_, _, err := runCtl(t, []string{
			"--url", ts.URL, "projects", "register", "--project", "42", "--token-env", "PROJECT_TOKEN", "--settings", settings,
		}, map[string]string{"REVIEW_ADMIN_TOKEN": "admin", "PROJECT_TOKEN": "token"}, ts.Client())
		if err == nil {
			t.Fatalf("settings %q accepted", settings)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("invalid settings reached the server")
	}
}

func TestInvalidServiceURLs(t *testing.T) {
	for _, value := range []string{
		"ftp://example.com", "https://user:pass@example.com", "https://example.com?x=1",
		"https://example.com/#fragment", "not-a-url", " https://example.com",
	} {
		_, _, err := runCtl(t, []string{"--url", value, "projects", "list"}, map[string]string{
			"REVIEW_ADMIN_TOKEN": "admin",
		}, nil)
		if err == nil {
			t.Errorf("--url %q accepted", value)
		}
	}
}

func TestHelpAndUsage(t *testing.T) {
	_, _, err := runCtl(t, []string{"--help"}, nil, nil)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error = %v", err)
	}
	if _, _, err := runCtl(t, nil, map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, nil); err == nil {
		t.Fatal("missing projects subcommand accepted")
	}
	if _, _, err := runCtl(t, []string{"projects", "bogus"}, map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, nil); err == nil {
		t.Fatal("unknown action accepted")
	}
	if _, _, err := runCtl(t, []string{"projects", "get", "abc"}, map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, nil); err == nil {
		t.Fatal("invalid id accepted")
	}
}

func TestInvalidJSONResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "not json")
	}))
	defer ts.Close()
	_, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "list"}, map[string]string{
		"REVIEW_ADMIN_TOKEN": "admin",
	}, ts.Client())
	if err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestAPIErrorEnvelope(t *testing.T) {
	var envelope errorEnvelope
	if err := json.Unmarshal([]byte(`{"error":{"code":"conflict","message":"already registered","request_id":"r"}}`), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "conflict" {
		t.Fatalf("envelope = %+v", envelope)
	}
}
