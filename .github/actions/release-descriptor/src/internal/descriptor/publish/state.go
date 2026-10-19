package publish

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Vibrant-America/promotion-controller/internal/descriptor"
)

const maxStateBytes = 1 << 20

// prepared is the prepared-publication record persisted in the CI run (§23.2 step 2): the canonical payload, the exact
// manifests and their digests and publicationStartedAt, then the journal position and head-write time as they become
// known. Retries in the same run attempt reuse its bytes and timestamp after validating them against this run's
// outputs.
type prepared struct {
	RunID       string
	RunAttempt  string
	Payload     []byte
	Manifest    []byte // canonical, subject-free
	StartedAt   string // publicationStartedAt
	Copies      []copyRecord
	Sequence    int64 // 0 until the entry is created
	EntryDigest string
	PublishedAt string // successful head write
}

type copyRecord struct {
	Image    string
	Manifest []byte
}

func statePath(dir, runID, runAttempt string) string {
	return filepath.Join(dir, "release-descriptor-"+runID+"-"+runAttempt+".json")
}

func (s prepared) encode() []byte {
	copies := make([]descriptor.Value, len(s.Copies))
	for i, c := range s.Copies {
		copies[i] = descriptor.Obj(
			descriptor.Member{Key: "image", Value: descriptor.Str(c.Image)},
			descriptor.Member{Key: "manifest", Value: descriptor.Str(string(c.Manifest))},
			descriptor.Member{Key: "manifestDigest", Value: descriptor.Str(descriptor.SHA256(c.Manifest))},
		)
	}
	return descriptor.Canonical(descriptor.Obj(
		descriptor.Member{Key: "v", Value: descriptor.Int(1)},
		descriptor.Member{Key: "runId", Value: descriptor.Str(s.RunID)},
		descriptor.Member{Key: "runAttempt", Value: descriptor.Str(s.RunAttempt)},
		descriptor.Member{Key: "payload", Value: descriptor.Str(string(s.Payload))},
		descriptor.Member{Key: "payloadDigest", Value: descriptor.Str(descriptor.SHA256(s.Payload))},
		descriptor.Member{Key: "manifest", Value: descriptor.Str(string(s.Manifest))},
		descriptor.Member{Key: "manifestDigest", Value: descriptor.Str(descriptor.SHA256(s.Manifest))},
		descriptor.Member{Key: "publicationStartedAt", Value: descriptor.Str(s.StartedAt)},
		descriptor.Member{Key: "copies", Value: descriptor.Arr(copies...)},
		descriptor.Member{Key: "journalSequence", Value: descriptor.Str(strconv.FormatInt(s.Sequence, 10))},
		descriptor.Member{Key: "journalEntryDigest", Value: descriptor.Str(s.EntryDigest)},
		descriptor.Member{Key: "publishedAt", Value: descriptor.Str(s.PublishedAt)},
	))
}

// loadState reads the record for this run attempt. A missing record is (zero, false, nil); an unreadable or malformed
// one is stateInvalid, never silently replaced.
func loadState(dir, runID, runAttempt string) (prepared, bool, error) {
	path := statePath(dir, runID, runAttempt)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return prepared{}, false, nil
	}
	data, err := readRegular(path, maxStateBytes, "state")
	if err != nil {
		return prepared{}, false, fail(CodeStateInvalid, "")
	}
	bad := func() (prepared, bool, error) { return prepared{}, false, fail(CodeStateInvalid, "") }
	v, err := descriptor.Parse(data, descriptor.Limits{Bytes: maxStateBytes, Depth: 4, Nodes: 1024})
	if err != nil {
		return bad()
	}
	f, ok := object(v, "v", "runId", "runAttempt", "payload", "payloadDigest", "manifest", "manifestDigest",
		"publicationStartedAt", "copies", "journalSequence", "journalEntryDigest", "publishedAt")
	if !ok || f["v"].Kind != descriptor.Number || f["v"].Int != 1 || f["copies"].Kind != descriptor.Array {
		return bad()
	}
	s := prepared{
		RunID:       f["runId"].Str,
		RunAttempt:  f["runAttempt"].Str,
		Payload:     []byte(f["payload"].Str),
		Manifest:    []byte(f["manifest"].Str),
		StartedAt:   f["publicationStartedAt"].Str,
		EntryDigest: f["journalEntryDigest"].Str,
		PublishedAt: f["publishedAt"].Str,
	}
	if s.Sequence, ok = descriptor.Sequence(f["journalSequence"].Str); !ok {
		return bad()
	}
	for _, item := range f["copies"].Items {
		c, ok := object(item, "image", "manifest", "manifestDigest")
		if !ok {
			return bad()
		}
		s.Copies = append(s.Copies, copyRecord{Image: c["image"].Str, Manifest: []byte(c["manifest"].Str)})
	}
	if s.RunID != runID || s.RunAttempt != runAttempt || !bytesEqual(s.encode(), data) {
		return bad()
	}
	return s, true, nil
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }

// save writes the record atomically with mode 0600: a temporary file in the same directory, then a rename.
func (s prepared) save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(CodeStateInvalid, "")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return fail(CodeStateInvalid, "")
	}
	tmp, err := os.CreateTemp(dir, ".release-descriptor-*")
	if err != nil {
		return fail(CodeStateInvalid, "")
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone after a successful rename
	if _, err := tmp.Write(s.encode()); err != nil {
		_ = tmp.Close() // the write error is the one reported
		return fail(CodeStateInvalid, "")
	}
	if err := tmp.Close(); err != nil {
		return fail(CodeStateInvalid, "")
	}
	if err := os.Rename(tmp.Name(), statePath(dir, s.RunID, s.RunAttempt)); err != nil {
		return fail(CodeStateInvalid, "")
	}
	return nil
}
