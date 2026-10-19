package descriptor

import (
	"bytes"
	"strconv"
)

// Journal bounds (§23.2).
const (
	journalSchema   = 1
	maxJournalBytes = 4 << 10
	maxJournalDepth = 4
	maxHeadNodes    = 32
	maxEntryNodes   = 64
)

// Head is the journal head payload. Sequence 0 has no entry digest; every later sequence names its entry's
// manifest digest.
type Head struct {
	Project     string
	Sequence    int64
	EntryDigest string // empty at sequence 0
}

// Entry is one immutable journal entry.
type Entry struct {
	Project          string
	Sequence         int64
	PreviousDigest   string // empty at sequence 1
	DescriptorDigest string
	PublicationTag   string
}

func nullable(s string) Value {
	if s == "" {
		return NullValue()
	}
	return Str(s)
}

func journalError(path string) error { return invalid(CodeJournalInvalid, path) }

// Validate checks a head's fields.
func (h Head) Validate() error {
	switch {
	case !DNSLabel(h.Project):
		return journalError("project")
	case h.Sequence < 0:
		return journalError("sequence")
	case (h.Sequence == 0) != (h.EntryDigest == ""), h.EntryDigest != "" && !Digest(h.EntryDigest):
		return journalError("entryDigest")
	}
	return nil
}

// Encode returns the canonical head payload.
func (h Head) Encode() ([]byte, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return Canonical(Obj(
		Member{"schemaVersion", Int(journalSchema)},
		Member{"project", Str(h.Project)},
		Member{"sequence", Str(strconv.FormatInt(h.Sequence, 10))},
		Member{"entryDigest", nullable(h.EntryDigest)},
	)), nil
}

// ParseHead strictly decodes canonical head bytes for project.
func ParseHead(data []byte, project string) (Head, error) {
	f, err := parseJournal(data, maxHeadNodes, "sequence", "entryDigest")
	if err != nil {
		return Head{}, err
	}
	h := Head{Project: f["project"].Str}
	var ok bool
	if h.Sequence, ok = sequence(f["sequence"]); !ok {
		return Head{}, journalError("sequence")
	}
	if h.EntryDigest, ok = digestOrNull(f["entryDigest"]); !ok {
		return Head{}, journalError("entryDigest")
	}
	return h, finishJournal(h.Validate(), h.Project, project, data, h.Encode)
}

// Validate checks an entry's fields.
func (e Entry) Validate() error {
	switch {
	case !DNSLabel(e.Project):
		return journalError("project")
	case e.Sequence < 1:
		return journalError("sequence")
	case (e.Sequence == 1) != (e.PreviousDigest == ""), e.PreviousDigest != "" && !Digest(e.PreviousDigest):
		return journalError("previousDigest")
	case !Digest(e.DescriptorDigest):
		return journalError("descriptorDigest")
	}
	if _, _, ok := ParseDescriptorTag(e.PublicationTag); !ok {
		return journalError("publicationTag")
	}
	return nil
}

// Encode returns the canonical entry payload.
func (e Entry) Encode() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return Canonical(Obj(
		Member{"schemaVersion", Int(journalSchema)},
		Member{"project", Str(e.Project)},
		Member{"sequence", Str(strconv.FormatInt(e.Sequence, 10))},
		Member{"previousDigest", nullable(e.PreviousDigest)},
		Member{"descriptorDigest", Str(e.DescriptorDigest)},
		Member{"publicationTag", Str(e.PublicationTag)},
	)), nil
}

// ParseEntry strictly decodes canonical entry bytes for project.
func ParseEntry(data []byte, project string) (Entry, error) {
	f, err := parseJournal(data, maxEntryNodes, "sequence", "previousDigest", "descriptorDigest", "publicationTag")
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Project: f["project"].Str, DescriptorDigest: f["descriptorDigest"].Str, PublicationTag: f["publicationTag"].Str}
	var ok bool
	if e.Sequence, ok = sequence(f["sequence"]); !ok {
		return Entry{}, journalError("sequence")
	}
	if e.PreviousDigest, ok = digestOrNull(f["previousDigest"]); !ok {
		return Entry{}, journalError("previousDigest")
	}
	if f["descriptorDigest"].Kind != String || f["publicationTag"].Kind != String {
		return Entry{}, journalError("")
	}
	return e, finishJournal(e.Validate(), e.Project, project, data, e.Encode)
}

func parseJournal(data []byte, nodes int, keys ...string) (map[string]Value, error) {
	if len(data) > maxJournalBytes {
		return nil, journalError("")
	}
	v, err := Parse(data, Limits{Bytes: maxJournalBytes, Depth: maxJournalDepth, Nodes: nodes})
	if err != nil {
		return nil, journalError("")
	}
	f, ok := fields(v, append([]string{"schemaVersion", "project"}, keys...))
	if !ok || f["project"].Kind != String {
		return nil, journalError("")
	}
	if f["schemaVersion"].Kind != Number || f["schemaVersion"].Int != journalSchema {
		return nil, invalid(CodeUnsupported, "schemaVersion")
	}
	return f, nil
}

func finishJournal(err error, got, want string, data []byte, encode func() ([]byte, error)) error {
	if err != nil {
		return err
	}
	if got != want {
		return invalid(CodeProjectMismatch, "project")
	}
	canonical, err := encode()
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, data) {
		return invalid(CodeNotCanonical, "")
	}
	return nil
}

func sequence(v Value) (int64, bool) {
	if v.Kind != String {
		return 0, false
	}
	return Sequence(v.Str)
}

func digestOrNull(v Value) (string, bool) {
	switch {
	case v.Kind == Null:
		return "", true
	case v.Kind == String && Digest(v.Str):
		return v.Str, true
	}
	return "", false
}
