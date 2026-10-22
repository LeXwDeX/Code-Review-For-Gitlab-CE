package mcp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrUnsafeEnv reports an environment entry that could change process
// behavior or expose platform credentials. The error never contains a value.
var ErrUnsafeEnv = errors.New("mcp: unsafe environment")

// ErrSensitiveEnv reports an ${NAME} expansion that would copy a secret from
// the process environment into configuration. The error names the variable but
// never its value.
var ErrSensitiveEnv = errors.New("mcp: sensitive environment expansion")

// ErrUnsafeWorkspace reports a workspace or home path that is missing, not a
// real directory, or escapes the workspace.
var ErrUnsafeWorkspace = errors.New("mcp: unsafe workspace path")

type envPolicy struct {
	lookup func(string) (string, bool)
	denied []string
}

func newEnvPolicy(lookup func(string) (string, bool), denied []string) envPolicy {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	clean := make([]string, 0, len(denied))
	for _, name := range denied {
		name = strings.TrimSpace(name)
		if name != "" {
			clean = append(clean, name)
		}
	}
	return envPolicy{lookup: lookup, denied: clean}
}

// build returns the child environment and its isolated home. Only PATH, an
// explicit x-review.env_allowlist, and explicit env values are inherited or
// passed. HOME is always replaced by a workspace-confined directory.
func (p envPolicy) build(cfg ServerConfig, workspace, server string) ([]string, string, error) {
	home, err := resolveHome(cfg, workspace, server)
	if err != nil {
		return nil, "", err
	}
	values := make(map[string]string)
	seen := make(map[string]string)
	set := func(name, value string) error {
		key := strings.ToUpper(name)
		if previous, ok := seen[key]; ok && previous != name {
			return fmt.Errorf("%w: duplicate variable", ErrUnsafeEnv)
		}
		seen[key] = name
		values[name] = value
		return nil
	}

	path := "/usr/local/bin:/usr/bin:/bin"
	if value, ok := p.lookup("PATH"); ok && value != "" {
		path = value
	}
	if err := set("PATH", path); err != nil {
		return nil, "", err
	}
	if err := set("HOME", home); err != nil {
		return nil, "", err
	}

	for _, name := range cfg.XReview.EnvAllowlist {
		if strings.EqualFold(name, "HOME") {
			continue
		}
		if p.inheritDenied(name) {
			continue
		}
		value, ok := p.lookup(name)
		if !ok {
			continue
		}
		if err := set(name, value); err != nil {
			return nil, "", err
		}
	}

	for name, value := range cfg.Env {
		if strings.EqualFold(name, "HOME") {
			return nil, "", fmt.Errorf("%w: HOME is isolated per workspace", ErrUnsafeEnv)
		}
		if p.deniedName(name) || dangerousName(name) {
			return nil, "", fmt.Errorf("%w: variable %s is not allowed", ErrUnsafeEnv, name)
		}
		if err := set(name, value); err != nil {
			return nil, "", err
		}
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		env = append(env, name+"="+values[name])
	}
	return env, home, nil
}

// inheritDenied rejects credentials and process-control variables even when an
// administrator lists them in env_allowlist.
func (p envPolicy) inheritDenied(name string) bool {
	if dangerousName(name) || sensitiveName(name) {
		return true
	}
	return p.deniedName(name)
}

// deniedName applies the caller's project-token and provider-key names to both
// inherited and explicitly configured variables.
func (p envPolicy) deniedName(name string) bool {
	return nameDenied(name, p.denied)
}

func nameDenied(name string, denied []string) bool {
	for _, entry := range denied {
		if strings.EqualFold(name, entry) {
			return true
		}
	}
	return false
}

func dangerousName(name string) bool {
	upper := strings.ToUpper(name)
	switch upper {
	case "GIT_ASKPASS", "SSH_AUTH_SOCK", "SSH_ASKPASS", "GIT_TERMINAL_PROMPT":
		return true
	}
	return strings.HasPrefix(upper, "LD_") ||
		strings.HasPrefix(upper, "DYLD_") ||
		strings.HasPrefix(upper, "GIT_CONFIG_")
}

func sensitiveName(name string) bool {
	upper := strings.ToUpper(name)
	for _, prefix := range []string{"REVIEW_", "GITLAB_", "ANTHROPIC_", "OPENAI_", "AWS_", "AZURE_", "GOOGLE_"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "API_KEY", "PRIVATE_KEY"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return strings.HasSuffix(upper, "_KEY")
}

func resolveHome(cfg ServerConfig, workspace, server string) (string, error) {
	base, err := prepareWorkspace(workspace)
	if err != nil {
		return "", err
	}
	home := cfg.XReview.Home
	if home == "" {
		home = filepath.Join(base, ".mcp", server, "home")
	} else if !filepath.IsAbs(home) {
		home = filepath.Join(base, home)
	}
	home = filepath.Clean(home)
	if home == base {
		return "", fmt.Errorf("%w: home must be below the workspace", ErrUnsafeWorkspace)
	}
	rel, err := filepath.Rel(base, home)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: home escapes the workspace", ErrUnsafeWorkspace)
	}
	if err := secureMkdir(base, home); err != nil {
		return "", err
	}
	return home, nil
}

func prepareWorkspace(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%w: workspace is required", ErrUnsafeWorkspace)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: workspace path is invalid", ErrUnsafeWorkspace)
	}
	absolute = filepath.Clean(absolute)
	if filepath.Dir(absolute) == absolute {
		return "", fmt.Errorf("%w: filesystem root is not a workspace", ErrUnsafeWorkspace)
	}
	info, err := os.Lstat(absolute)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(absolute, 0o700); err != nil {
			return "", fmt.Errorf("mcp: create workspace: %w", err)
		}
		info, err = os.Lstat(absolute)
	case err != nil:
		return "", fmt.Errorf("mcp: inspect workspace: %w", err)
	}
	if err != nil {
		return "", fmt.Errorf("mcp: inspect workspace: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%w: workspace must be a real directory", ErrUnsafeWorkspace)
	}
	return absolute, nil
}

// secureMkdir creates dir below base one component at a time. Existing
// components must be real directories, never symlinks.
func secureMkdir(base, dir string) error {
	cleanBase := filepath.Clean(base)
	if cleanBase == dir {
		return nil
	}
	if filepath.Dir(cleanBase) == cleanBase {
		return fmt.Errorf("%w: base is a filesystem root", ErrUnsafeWorkspace)
	}
	info, err := os.Lstat(cleanBase)
	if err != nil {
		return fmt.Errorf("mcp: inspect workspace: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: workspace must be a real directory", ErrUnsafeWorkspace)
	}
	rel, err := filepath.Rel(cleanBase, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: path escapes the workspace", ErrUnsafeWorkspace)
	}
	current := cleanBase
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%w: invalid path component", ErrUnsafeWorkspace)
		}
		if strings.EqualFold(part, ".git") {
			return fmt.Errorf("%w: .git paths are reserved", ErrUnsafeWorkspace)
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("mcp: create directory: %w", err)
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return fmt.Errorf("mcp: inspect directory: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: path component is not a real directory", ErrUnsafeWorkspace)
		}
	}
	return nil
}
