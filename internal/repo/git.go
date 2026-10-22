// Package repo wraps the small set of git operations the platform needs:
// a bare mirror, detached worktrees and read-only revision queries.
//
// Repositories are untrusted. Every command runs without a shell, with a
// whitelisted environment, repository and global hooks disabled, external
// diff/textconv helpers disabled, file and ssh transports blocked, and
// redirects turned off. Tokens travel only in an http.extraHeader that is
// injected through Git's environment configuration.
package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	defaultBin   = "git"
	maxTokenLen  = 4096
	maxRefLen    = 1024
	maxURLLen    = 2048
	maxPathLen   = 4096
	gitWaitDelay = 5 * time.Second
)

// Auth carries a project token for one operation. An empty token means
// anonymous access.
type Auth struct {
	Token string
}

// Git runs git commands. The zero value is usable.
type Git struct {
	Bin     string
	BaseEnv []string
}

// Error is a sanitized git failure. It never contains stderr, argv or a URL:
// stderr can echo credentials and argv can contain repository paths.
type Error struct {
	Op       string
	ExitCode int
}

func (e *Error) Error() string {
	if e == nil {
		return "repo: git failed"
	}
	if e.Op == "" {
		return "repo: git failed"
	}
	if e.ExitCode > 0 {
		return fmt.Sprintf("repo: %s failed (exit %d)", e.Op, e.ExitCode)
	}
	return fmt.Sprintf("repo: %s failed", e.Op)
}

var (
	// ErrInvalidURL reports a remote URL outside the allowed http(s) shape.
	ErrInvalidURL = errors.New("repo: invalid remote URL")
	// ErrInvalidRef reports a ref or refspec that could alter git options or
	// address an unexpected namespace.
	ErrInvalidRef = errors.New("repo: invalid ref")
	// ErrInvalidSHA reports a revision that is not a full object ID.
	ErrInvalidSHA = errors.New("repo: invalid object ID")
	// ErrUnsafePath reports a relative, escaping, or symlinked path.
	ErrUnsafePath = errors.New("repo: unsafe path")
	// ErrNotMirror reports an existing directory that is not a bare mirror.
	ErrNotMirror = errors.New("repo: directory is not a bare mirror")
	// ErrPathExists reports an existing worktree target.
	ErrPathExists = errors.New("repo: path already exists")
)

var (
	shaRE = regexp.MustCompile(`^[0-9a-fA-F]{40}$|^[0-9a-fA-F]{64}$`)
	refRE = regexp.MustCompile(`^refs/[A-Za-z0-9._/+-]+$`)
)

func (g *Git) bin() string {
	if g != nil && g.Bin != "" {
		return g.Bin
	}
	return defaultBin
}

// ValidateSHA checks that value is a complete SHA-1 or SHA-256 object ID.
func ValidateSHA(value string) error {
	if !shaRE.MatchString(value) {
		return ErrInvalidSHA
	}
	return nil
}

// NormalizeRef returns a fully qualified ref. A short branch name gets the
// refs/heads/ prefix; a full refs/... name is validated as is.
func NormalizeRef(ref string) (string, error) {
	if ref == "" || len(ref) > maxRefLen || strings.ContainsAny(ref, " \t\r\n\x00") {
		return "", ErrInvalidRef
	}
	if strings.HasPrefix(ref, "refs/") {
		if err := validateRef(ref); err != nil {
			return "", err
		}
		return ref, nil
	}
	if ref == "HEAD" || strings.HasPrefix(ref, "-") {
		return "", ErrInvalidRef
	}
	full := "refs/heads/" + ref
	if err := validateRef(full); err != nil {
		return "", err
	}
	return full, nil
}

func validateRef(ref string) error {
	if ref == "" || len(ref) > maxRefLen || !refRE.MatchString(ref) {
		return ErrInvalidRef
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, "@{") ||
		strings.Contains(ref, "//") || strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".") {
		return ErrInvalidRef
	}
	for _, segment := range strings.Split(strings.TrimPrefix(ref, "refs/"), "/") {
		if segment == "" || segment == "." || segment == ".." || strings.HasSuffix(segment, ".lock") {
			return ErrInvalidRef
		}
	}
	return nil
}

func validateRefspec(refspec string) error {
	if refspec == "" || refspec == "+" {
		return ErrInvalidRef
	}
	spec := strings.TrimPrefix(refspec, "+")
	if strings.Contains(spec, "*") {
		return ErrInvalidRef
	}
	src, dst, ok := strings.Cut(spec, ":")
	if !ok || src == "" || dst == "" {
		return ErrInvalidRef
	}
	if err := validateRef(src); err != nil {
		return err
	}
	return validateRef(dst)
}

func validateRevision(revision string) error {
	if ValidateSHA(revision) == nil {
		return nil
	}
	if revision == "HEAD" {
		return nil
	}
	if strings.HasPrefix(revision, "refs/") {
		return validateRef(revision)
	}
	return ErrInvalidRef
}

func validateRemoteURL(raw string) error {
	if raw == "" || len(raw) > maxURLLen || strings.TrimSpace(raw) != raw {
		return ErrInvalidURL
	}
	if strings.ContainsAny(raw, " \t\r\n\x00\\") {
		return ErrInvalidURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return ErrInvalidURL
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return ErrInvalidURL
	}
	if u.User != nil || u.Host == "" || u.Hostname() == "" {
		return ErrInvalidURL
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return ErrInvalidURL
	}
	if u.Path == "" || u.Path == "/" {
		return ErrInvalidURL
	}
	return nil
}

func validateAbsPath(path string) error {
	if path == "" || len(path) > maxPathLen || !filepath.IsAbs(path) {
		return ErrUnsafePath
	}
	if strings.ContainsAny(path, "\r\n\x00") {
		return ErrUnsafePath
	}
	return nil
}

func requireExistingDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("repo: inspect path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrUnsafePath
	}
	return nil
}

// run executes git with the hardened environment and returns stdout. Errors
// are reduced to an operation name and exit status.
func (g *Git) run(ctx context.Context, op, dir string, a Auth, args ...string) ([]byte, error) {
	env, err := g.buildEnv(a)
	if err != nil {
		return nil, wrapEnvError(err)
	}
	full := make([]string, 0, len(args)+len(globalArgs())+2)
	if dir != "" {
		full = append(full, "-C", dir)
	}
	full = append(full, globalArgs()...)
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, g.bin(), full...)
	cmd.Env = env
	cmd.WaitDelay = gitWaitDelay
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard

	runErr := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("repo: %s: %w", op, ctxErr)
	}
	if runErr == nil {
		return stdout.Bytes(), nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return nil, &Error{Op: op, ExitCode: exitErr.ExitCode()}
	}
	return nil, fmt.Errorf("repo: %s: %w", op, runErr)
}

// EnsureMirror initializes a bare mirror, or updates the origin of an
// existing one. Existing non-empty directories that are not bare repositories
// are left untouched and reported as ErrNotMirror. No clone is performed.
func (g *Git) EnsureMirror(ctx context.Context, dir, remoteURL string, a Auth) error {
	if err := validateRemoteURL(remoteURL); err != nil {
		return err
	}
	if err := validateAbsPath(dir); err != nil {
		return err
	}

	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("repo: create mirror: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("repo: secure mirror: %w", err)
		}
	case err != nil:
		return fmt.Errorf("repo: inspect mirror: %w", err)
	default:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ErrUnsafePath
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("repo: inspect mirror: %w", err)
	}
	if len(entries) == 0 {
		if _, err := g.run(ctx, "init mirror", "", Auth{}, "init", "--bare", "--quiet", dir); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("repo: secure mirror: %w", err)
		}
	} else {
		out, err := g.run(ctx, "verify mirror", "", Auth{}, "--git-dir="+dir, "rev-parse", "--is-bare-repository")
		if err != nil || strings.TrimSpace(string(out)) != "true" {
			return ErrNotMirror
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("repo: secure mirror: %w", err)
		}
	}

	if _, err := g.run(ctx, "read origin", "", Auth{}, "--git-dir="+dir, "config", "--get", "remote.origin.url"); err != nil {
		if _, addErr := g.run(ctx, "add origin", "", Auth{}, "--git-dir="+dir, "remote", "add", "origin", remoteURL); addErr != nil {
			return addErr
		}
		return nil
	}
	_, err = g.run(ctx, "update origin", "", Auth{}, "--git-dir="+dir, "remote", "set-url", "origin", remoteURL)
	return err
}

// Fetch fetches refspecs from origin into the mirror. Callers must pass full
// refspecs; force updates need an explicit "+" prefix.
func (g *Git) Fetch(ctx context.Context, mirror string, a Auth, refspecs ...string) error {
	if err := validateAbsPath(mirror); err != nil {
		return err
	}
	if err := requireExistingDir(mirror); err != nil {
		return err
	}
	for _, refspec := range refspecs {
		if err := validateRefspec(refspec); err != nil {
			return err
		}
	}
	args := []string{"fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--", "origin"}
	args = append(args, refspecs...)
	_, err := g.run(ctx, "fetch", mirror, a, args...)
	return err
}

// AddWorktree creates a detached worktree for sha at dir.
func (g *Git) AddWorktree(ctx context.Context, mirror, dir, sha string) error {
	if err := validateAbsPath(mirror); err != nil {
		return err
	}
	if err := validateAbsPath(dir); err != nil {
		return err
	}
	if err := ValidateSHA(sha); err != nil {
		return err
	}
	if err := requireExistingDir(mirror); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
		return ErrPathExists
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("repo: inspect worktree: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return fmt.Errorf("repo: create worktree parent: %w", err)
	}
	if _, err := g.run(ctx, "add worktree", mirror, Auth{}, "worktree", "add", "--detach", "--", dir, sha); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("repo: secure worktree: %w", err)
	}
	return nil
}

// RemoveWorktree removes a registered linked worktree. It is a no-op when the
// worktree directory is already gone.
func (g *Git) RemoveWorktree(ctx context.Context, mirror, dir string) error {
	if err := validateAbsPath(mirror); err != nil {
		return err
	}
	if err := validateAbsPath(dir); err != nil {
		return err
	}
	if err := requireExistingDir(mirror); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		_, pruneErr := g.run(ctx, "prune worktrees", mirror, Auth{}, "worktree", "prune")
		return pruneErr
	}
	if err != nil {
		return fmt.Errorf("repo: inspect worktree: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ErrUnsafePath
	}
	_, err = g.run(ctx, "remove worktree", mirror, Auth{}, "worktree", "remove", "--force", "--", dir)
	return err
}

// Checkout force-checks out sha in an existing platform-owned worktree.
func (g *Git) Checkout(ctx context.Context, dir, sha string) error {
	if err := validateAbsPath(dir); err != nil {
		return err
	}
	if err := ValidateSHA(sha); err != nil {
		return err
	}
	if err := requireExistingDir(dir); err != nil {
		return err
	}
	_, err := g.run(ctx, "checkout", dir, Auth{}, "checkout", "--quiet", "--force", "--detach", sha)
	return err
}

// Resolve returns the full commit ID for a ref, HEAD or full SHA.
func (g *Git) Resolve(ctx context.Context, dir, ref string) (string, error) {
	if err := validateAbsPath(dir); err != nil {
		return "", err
	}
	if err := validateRevision(ref); err != nil {
		return "", err
	}
	if err := requireExistingDir(dir); err != nil {
		return "", err
	}
	out, err := g.run(ctx, "resolve", dir, Auth{}, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if err := ValidateSHA(sha); err != nil {
		return "", fmt.Errorf("repo: resolve: %w", err)
	}
	return strings.ToLower(sha), nil
}

// ChangedFiles lists paths changed between two revisions, without rename
// detection so a rename appears as its old and new path. The result is
// ordered and includes no empty entries.
func (g *Git) ChangedFiles(ctx context.Context, dir, from, to string) ([]string, error) {
	if err := validateAbsPath(dir); err != nil {
		return nil, err
	}
	if err := validateRevision(from); err != nil {
		return nil, err
	}
	if err := validateRevision(to); err != nil {
		return nil, err
	}
	if err := requireExistingDir(dir); err != nil {
		return nil, err
	}
	out, err := g.run(ctx, "changed files", dir, Auth{},
		"diff", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv", "-z", from, to)
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(out, []byte{0})
	files := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		files = append(files, string(part))
	}
	return files, nil
}

// IsAncestor reports whether ancestor is an ancestor of descendant. A
// non-ancestor is a normal false result, not an error.
func (g *Git) IsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	if err := validateAbsPath(dir); err != nil {
		return false, err
	}
	if err := validateRevision(ancestor); err != nil {
		return false, err
	}
	if err := validateRevision(descendant); err != nil {
		return false, err
	}
	if err := requireExistingDir(dir); err != nil {
		return false, err
	}
	_, err := g.run(ctx, "is ancestor", dir, Auth{},
		"merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var gitErr *Error
	if errors.As(err, &gitErr) && gitErr.ExitCode == 1 {
		return false, nil
	}
	return false, err
}
