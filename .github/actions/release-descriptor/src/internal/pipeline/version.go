package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// VersionFormat is the "v" of a version's source, expansion and provenance (docs/design/05-storage.md §5.5).
const VersionFormat = 1

// Version is the content of an immutable pipeline version (docs/design/14-config-versions.md §14.1). Its JSON values
// are what pipeline_version stores.
type Version struct {
	Source     json.RawMessage // the decoded document plus "v": the file without its comments and formatting
	Expanded   json.RawMessage // {"v", owner, source, stages}: the effective document after expansion
	Provenance json.RawMessage // {"v", "<JSON pointer into Expanded>": {"from", "path"}}, one per leaf (§14.4)
	Digest     string          // the version identity: hex sha256 of the canonical JSON of the three
}

// Provenance sources (§14.4). Template sources additionally carry their name.
const (
	FromProject = "project"
	FromBuiltin = "default:builtin"
)

// Origin is one provenance entry: where a leaf of the expansion came from, and its path in that file. Paths name
// stages instead of indexing them (§14.4); a built-in default has no path.
type Origin struct {
	From string `json:"from"`
	Path string `json:"path,omitempty"`
}

type expandedDoc struct {
	V      int     `json:"v"`
	Owner  Owner   `json:"owner"`
	Source Source  `json:"source"`
	Stages []Stage `json:"stages"`
}

// NewVersion decodes data (Decode) and returns the pipeline with its version content. Two files that differ only in
// comments, key order, quoting or indentation have the same Version.
func NewVersion(data []byte) (Pipeline, Version, error) {
	return Expand(context.Background(), data, Platform{}, 0)
}

// Digest is the version identity of §14.1: the hex sha256 of the canonical JSON (RFC 8785) of
// {"source", "expanded", "provenance"}. It hashes the values as stored, so it can be recomputed from a row.
func Digest(source, expanded, provenance json.RawMessage) (string, error) {
	return digestContext(context.Background(), source, expanded, provenance)
}

func digestContext(ctx context.Context, source, expanded, provenance json.RawMessage) (string, error) {
	if err := ExpansionStep(ctx, int64(len(source)+len(expanded)+len(provenance))); err != nil {
		return "", err
	}
	doc, err := json.Marshal(map[string]json.RawMessage{"source": source, "expanded": expanded, "provenance": provenance})
	if err != nil {
		return "", fmt.Errorf("pipeline: digest: %w", err)
	}
	c, err := canonicalContext(ctx, doc)
	if err != nil {
		return "", fmt.Errorf("pipeline: digest: %w", err)
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), nil
}

// withFormat adds "v" to the decoded document. The document's own fields are checked, so it has no "v" of its own.
func withFormat(raw json.RawMessage) (json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("pipeline: source: %w", err)
	}
	doc["v"] = json.RawMessage(strconv.Itoa(VersionFormat))
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("pipeline: source: %w", err)
	}
	return b, nil
}

// pointerToken escapes a key for a JSON pointer (RFC 6901).
func pointerToken(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}
