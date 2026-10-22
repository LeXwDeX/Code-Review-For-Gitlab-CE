// Command reviewctl is a thin HTTP client for the M0 management API.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultURL     = "http://localhost:8080"
	defaultTimeout = 30 * time.Second
	maxResponse    = 4 << 20
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr, nil); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "reviewctl:", err)
		os.Exit(1)
	}
}

type globals struct {
	serviceURL  string
	apiTokenEnv string
	timeout     time.Duration
	apiToken    string
	httpClient  *http.Client
	stdout      io.Writer
	stderr      io.Writer
	getenv      func(string) string
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, client *http.Client) error {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	g := &globals{stdout: stdout, stderr: stderr, getenv: getenv}

	global := flag.NewFlagSet("reviewctl", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() {
		fmt.Fprintln(stderr, "Usage: reviewctl [--url URL] [--api-token-env NAME] projects <register|list|get> [options]")
		global.PrintDefaults()
	}
	global.StringVar(&g.serviceURL, "url", defaultURL, "service base URL")
	global.StringVar(&g.apiTokenEnv, "api-token-env", "REVIEW_ADMIN_TOKEN", "environment variable holding the admin token")
	global.DurationVar(&g.timeout, "timeout", defaultTimeout, "HTTP timeout")
	if err := global.Parse(args); err != nil {
		return err
	}
	if err := g.prepare(getenv, client); err != nil {
		return err
	}
	rest := global.Args()
	if len(rest) == 0 || rest[0] != "projects" {
		global.Usage()
		return errors.New("projects subcommand is required")
	}
	return g.runProjects(ctx, rest[1:])
}

func (g *globals) prepare(getenv func(string) string, client *http.Client) error {
	normalized, err := validateServiceURL(g.serviceURL)
	if err != nil {
		return err
	}
	g.serviceURL = normalized
	if strings.TrimSpace(g.apiTokenEnv) == "" || !envNameRE.MatchString(g.apiTokenEnv) {
		return errors.New("--api-token-env must be a valid environment variable name")
	}
	g.apiToken = getenv(g.apiTokenEnv)
	if strings.TrimSpace(g.apiToken) == "" {
		return fmt.Errorf("environment variable %s is not set or empty", g.apiTokenEnv)
	}
	if g.timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	if client == nil {
		client = &http.Client{}
	}
	copied := *client
	copied.Timeout = g.timeout
	copied.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	g.httpClient = &copied
	return nil
}

func validateServiceURL(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", errors.New("--url must be an absolute http(s) URL without credentials, query, or fragment")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		(u.Scheme != "http" && u.Scheme != "https") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("--url must be an absolute http(s) URL without credentials, query, or fragment")
	}
	return strings.TrimRight(raw, "/"), nil
}

func (g *globals) runProjects(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("projects action is required")
	}
	switch args[0] {
	case "register":
		return g.register(ctx, args[1:])
	case "list":
		if len(args) != 1 {
			return errors.New("projects list takes no arguments")
		}
		raw, err := g.request(ctx, http.MethodGet, "/api/v1/projects", nil)
		if err != nil {
			return err
		}
		return writeRaw(g.stdout, raw)
	case "get":
		if len(args) != 2 {
			return errors.New("projects get requires exactly one project ID")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 0 {
			return errors.New("project ID must be a positive integer")
		}
		raw, err := g.request(ctx, http.MethodGet, "/api/v1/projects/"+strconv.FormatInt(id, 10), nil)
		if err != nil {
			return err
		}
		return writeRaw(g.stdout, raw)
	default:
		return fmt.Errorf("unknown projects action %q", args[0])
	}
}

type registerRequest struct {
	Project     string          `json:"project"`
	Token       string          `json:"token"`
	Profile     string          `json:"profile,omitempty"`
	AutoWebhook bool            `json:"auto_webhook,omitempty"`
	Settings    json.RawMessage `json:"settings,omitempty"`
}

func (g *globals) register(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reviewctl projects register", flag.ContinueOnError)
	fs.SetOutput(g.stderr)
	var (
		project     string
		tokenEnv    string
		profile     string
		autoWebhook bool
		settings    string
	)
	fs.StringVar(&project, "project", "", "GitLab project ID or namespace/path")
	fs.StringVar(&tokenEnv, "token-env", "", "environment variable holding the project token")
	fs.StringVar(&profile, "profile", "", "review profile")
	fs.BoolVar(&autoWebhook, "auto-webhook", false, "create the GitLab webhook")
	fs.StringVar(&settings, "settings", "", "JSON project settings")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("projects register takes no positional arguments")
	}
	if project == "" {
		return errors.New("--project is required")
	}
	if tokenEnv == "" || !envNameRE.MatchString(tokenEnv) {
		return errors.New("--token-env must be a valid environment variable name")
	}
	token := g.getenv(tokenEnv)
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("environment variable %s is not set or empty", tokenEnv)
	}
	var rawSettings json.RawMessage
	if settings != "" {
		if !json.Valid([]byte(settings)) {
			return errors.New("--settings must be valid JSON")
		}
		trimmed := bytes.TrimSpace([]byte(settings))
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return errors.New("--settings must be a JSON object")
		}
		rawSettings = json.RawMessage(trimmed)
	}
	body := registerRequest{
		Project:     project,
		Token:       token,
		Profile:     profile,
		AutoWebhook: autoWebhook,
		Settings:    rawSettings,
	}
	raw, err := g.request(ctx, http.MethodPost, "/api/v1/projects", body)
	if err != nil {
		return err
	}
	return writeRaw(g.stdout, raw)
}

func (g *globals) request(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, errors.New("encode request: invalid input")
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.serviceURL+path, reader)
	if err != nil {
		return nil, errors.New("build request: invalid URL")
	}
	req.Header.Set("Authorization", "Bearer "+g.apiToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %s", sanitizeTransportError(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, errors.New("read response: transport error")
	}
	if len(data) > maxResponse {
		return nil, errors.New("read response: response is too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, responseError(resp.StatusCode, data)
	}
	if !json.Valid(data) {
		return nil, errors.New("server returned invalid JSON")
	}
	return json.RawMessage(data), nil
}

type errorEnvelope struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func responseError(status int, body []byte) error {
	var envelope errorEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Code != "" {
		message := fmt.Sprintf("server returned HTTP %d (%s)", status, envelope.Error.Code)
		if envelope.Error.RequestID != "" {
			message += " request_id=" + envelope.Error.RequestID
		}
		if envelope.Error.Message != "" {
			message += ": " + envelope.Error.Message
		}
		return errors.New(message)
	}
	// Do not echo arbitrary response text; it may contain upstream details.
	return fmt.Errorf("server returned HTTP %d", status)
}

func sanitizeTransportError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return "timeout"
	}
	return "transport error"
}

func writeRaw(w io.Writer, raw json.RawMessage) error {
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, raw, "", "  "); err != nil {
		return errors.New("server returned invalid JSON")
	}
	formatted.WriteByte('\n')
	_, err := w.Write(formatted.Bytes())
	return err
}
