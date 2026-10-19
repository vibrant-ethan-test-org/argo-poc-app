package publish

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// readRegular reads at most limit bytes of a regular file. A symlink, directory, device, FIFO or socket, a file that
// changes identity between the check and the open, and a file over limit all fail with fileInvalid (§23.4). The
// detail names the input, never the path or content.
func readRegular(path string, limit int64, detail string) ([]byte, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return nil, fail(CodeInputInvalid, detail)
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > limit {
		return nil, fail(CodeFileInvalid, detail)
	}
	// O_NOFOLLOW refuses a symlink swapped in after Lstat; O_NONBLOCK keeps a swapped-in FIFO from blocking the open.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: the path is an action input; the file is checked to be regular and bounded
	if err != nil {
		return nil, fail(CodeFileInvalid, detail)
	}
	defer func() { _ = f.Close() }() // read-only; ReadAll reports read failures
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, fail(CodeFileInvalid, detail)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, fail(CodeFileInvalid, detail)
	}
	return data, nil
}

// outputValue is the grammar of every value the action writes: digests, references, timestamps, decimals and
// booleans. It excludes CR, LF, '%' and anything that could start another output or a workflow command.
var outputValue = regexp.MustCompile(`^[A-Za-z0-9:/._@-]+$`)

const maxOutputValue = 1024

var outputName = regexp.MustCompile(`^[a-z][a-z-]{0,63}$`)

// Output is one name=value pair for the runner's output file.
type Output struct{ Name, Value string }

// writeOutputs appends validated outputs to the runner's output file (GITHUB_OUTPUT) in one write. The file must
// already exist as a regular file, as the runner creates it; a symlink or other file type is refused. Every name and
// value is checked against its grammar first, so no input can inject another output (§23.4).
func writeOutputs(path string, outputs []Output) error {
	var b strings.Builder
	for _, o := range outputs {
		if !outputName.MatchString(o.Name) || len(o.Value) > maxOutputValue || !outputValue.MatchString(o.Value) {
			return fail(CodeOutputInvalid, "value")
		}
		b.WriteString(o.Name + "=" + o.Value + "\n")
	}
	if path == "" || !filepath.IsAbs(path) {
		return fail(CodeOutputInvalid, "GITHUB_OUTPUT")
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return fail(CodeOutputInvalid, "GITHUB_OUTPUT")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: the runner's own output file, checked to be regular
	if err != nil {
		return fail(CodeOutputInvalid, "GITHUB_OUTPUT")
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		_ = f.Close() // nothing was written
		return fail(CodeOutputInvalid, "GITHUB_OUTPUT")
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close() // the write error is the one reported
		return fail(CodeOutputInvalid, "GITHUB_OUTPUT")
	}
	if err := f.Close(); err != nil {
		return fail(CodeOutputInvalid, "GITHUB_OUTPUT")
	}
	return nil
}

// EscapeLog makes a validated value safe to print in an Actions log: '%', CR, LF and ':' are percent-encoded, so no
// value can form a workflow command (`::`) or start a new line (§23.4).
func EscapeLog(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A").Replace(s)
}
