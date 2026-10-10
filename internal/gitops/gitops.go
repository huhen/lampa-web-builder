// Package gitops queries the upstream repository and maintains working copies
// of it under the data directory.
package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/huhen/lampa-web-builder/internal/execrun"
)

// Client talks to one upstream repository.
type Client struct {
	Repo   string
	Branch string
	Runner execrun.Runner
	// LsRemoteTimeout bounds a single ls-remote call.
	LsRemoteTimeout time.Duration
}

// NewClient returns a Client; ls-remote timeout defaults to 60s.
func NewClient(repo, branch string, r execrun.Runner) *Client {
	return &Client{Repo: repo, Branch: branch, Runner: r, LsRemoteTimeout: time.Minute}
}

// LsRemoteHead returns the current head commit of the branch. Cheap: no clone
// involved.
func (c *Client) LsRemoteHead(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.LsRemoteTimeout)
	defer cancel()
	var out strings.Builder
	// Query the full refspec: a bare branch name matches by tail suffix, so
	// "main" would also pick up e.g. "feature/main".
	if err := c.Runner.Run(ctx, "", &out, "git", "ls-remote", c.Repo, "refs/heads/"+c.Branch); err != nil {
		return "", fmt.Errorf("git ls-remote %s %s: %w", c.Repo, c.Branch, err)
	}
	line := strings.TrimSpace(out.String())
	// A missing branch is not a git error: ls-remote exits 0 with no output.
	if line == "" {
		return "", fmt.Errorf("branch %s not found in %s", c.Branch, c.Repo)
	}
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	hash, _, _ := strings.Cut(line, "\t")
	hash = strings.TrimSpace(hash)
	if len(hash) != 40 {
		return "", fmt.Errorf("git ls-remote: unexpected output %q", out.String())
	}
	return hash, nil
}

// EnsureCopy clones the upstream into dir unless a complete working copy
// already exists there. A surviving .git directory is not proof of a complete
// copy: a process killed mid-clone leaves a truncated one behind, so readiness
// is decided by resolving HEAD to a commit. The clone goes into a sibling temp
// directory and is published with an atomic rename, so dir is either absent or
// complete (issue #10).
func (c *Client) EnsureCopy(ctx context.Context, dir string) error {
	if c.copyReady(ctx, dir) {
		return nil
	}
	tmp := dir + ".tmp"
	// A crash can leave a stale temp behind; the copy below is built from
	// scratch, so anything under that name is disposable.
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("clear stale clone %s: %w", tmp, err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("create parent of %s: %w", dir, err)
	}
	var out bytes.Buffer
	if err := c.Runner.Run(ctx, "", &out, "git", "clone", c.Repo, tmp); err != nil {
		// Cleanup a partial clone so a retry can start fresh.
		os.RemoveAll(tmp)
		return fmt.Errorf("git clone %s: %w%s", c.Repo, err, outTail(&out))
	}
	// Publish atomically: a complete replacement exists in tmp before the
	// rejected leftover (it just failed copyReady) is removed, and dir is only
	// ever produced by the rename below.
	if err := os.RemoveAll(dir); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("clear %s: %w", dir, err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("publish %s: %w", dir, err)
	}
	return nil
}

// copyReady reports whether dir holds a complete working copy: HEAD resolves
// to a commit, which a truncated clone cannot do. A missing dir, a dir without
// .git and an unfinished .git all fail the same way.
func (c *Client) copyReady(ctx context.Context, dir string) bool {
	var out bytes.Buffer
	return c.Runner.Run(ctx, dir, &out, "git", "rev-parse", "--verify", "--quiet", "HEAD^{commit}") == nil
}

// Fetch updates the working copy in dir from upstream, bringing the branch
// head (and its objects) into the copy.
func (c *Client) Fetch(ctx context.Context, dir string) error {
	var out bytes.Buffer
	if err := c.Runner.Run(ctx, dir, &out, "git", "fetch", "origin", c.Branch); err != nil {
		return fmt.Errorf("git fetch origin %s: %w%s", c.Branch, err, outTail(&out))
	}
	return nil
}

// outTail returns the last chunk of captured command output for error
// messages, or an empty string when the command printed nothing.
func outTail(buf *bytes.Buffer) string {
	s := strings.TrimSpace(buf.String())
	if s == "" {
		return ""
	}
	if len(s) > 300 {
		s = s[len(s)-300:]
	}
	return ": " + s
}
