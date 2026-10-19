package publish

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// SourceReader reads the pipeline file from the exact source commit (§23.4).
type SourceReader interface {
	// Pipeline returns the bytes of path at commit, after checking that the checkout's HEAD is commit and that the
	// checked-out file has the same bytes.
	Pipeline(ctx context.Context, workspace, commit, path string) ([]byte, error)
}

// pipelinePath is a repository-relative file path: no leading slash or dash, no traversal, no colon or other
// revision syntax, at most 255 bytes.
var pipelinePath = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)*$`)

func validPipelinePath(p string) bool {
	if len(p) > 255 || !pipelinePath.MatchString(p) {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// GitSource reads from the checkout with git, which runs without a shell, with fixed arguments built from validated
// values, a minimal environment and a deadline per command. Nothing submitted is executed.
type GitSource struct {
	Git     string        // git binary; "git" when empty
	Timeout time.Duration // per command; 30s when zero
}

func (g GitSource) run(ctx context.Context, dir string, limit int64, args ...string) ([]byte, error) {
	timeout := g.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	bin := g.Git
	if bin == "" {
		bin = "git"
	}
	// Replacement objects and grafts could make the commit ID name another tree, so they are disabled: the pipeline
	// is read from the exact commit's own tree. No system or global configuration is read either.
	cmd := exec.CommandContext(ctx, bin, append([]string{"--no-replace-objects", "-C", dir}, args...)...) //nolint:gosec // G204: fixed git subcommands with validated arguments, no shell
	cmd.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1",
		"GIT_TERMINAL_PROMPT=0", "HOME=" + dir, "PATH=/usr/bin:/bin:/usr/local/bin"}
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, limit+1))
	if int64(len(out)) > limit {
		_ = cmd.Process.Kill() // over the limit: stop reading and reap below
	}
	waitErr := cmd.Wait()
	switch {
	case int64(len(out)) > limit:
		return nil, fail(CodeFileInvalid, "pipeline-file")
	case readErr != nil:
		return nil, readErr
	case waitErr != nil:
		return nil, waitErr
	}
	return out, nil
}

// Pipeline implements SourceReader.
func (g GitSource) Pipeline(ctx context.Context, workspace, commit, path string) ([]byte, error) {
	if !validPipelinePath(path) || !filepath.IsAbs(workspace) {
		return nil, fail(CodeInputInvalid, "pipeline-file")
	}
	head, err := g.run(ctx, workspace, 256, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(string(head)) != commit {
		return nil, fail(CodePipelineMismatch, "checkout")
	}
	// The tree entry must be a regular file (mode 100644 or 100755), never a symlink or submodule.
	entry, err := g.run(ctx, workspace, 4096, "ls-tree", "-z", "--full-tree", commit, "--", path)
	if err != nil {
		return nil, fail(CodePipelineMismatch, "pipeline-file")
	}
	meta, name, ok := strings.Cut(strings.TrimSuffix(string(entry), "\x00"), "\t")
	fieldsOf := strings.Fields(meta)
	if !ok || name != path || len(fieldsOf) != 3 || fieldsOf[1] != "blob" || (fieldsOf[0] != "100644" && fieldsOf[0] != "100755") {
		return nil, fail(CodeFileInvalid, "pipeline-file")
	}
	blob, err := g.run(ctx, workspace, maxPipelineBytes, "cat-file", "blob", fieldsOf[2])
	if err != nil {
		return nil, failWith(CodeFileInvalid, "pipeline-file", err)
	}
	checkout, err := readRegular(filepath.Join(workspace, filepath.FromSlash(path)), maxPipelineBytes, "pipeline-file")
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(checkout, blob) {
		return nil, fail(CodePipelineMismatch, "pipeline-file")
	}
	return blob, nil
}
