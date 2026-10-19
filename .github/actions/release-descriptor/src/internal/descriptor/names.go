package descriptor

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Syntax shared by the wire schema (§23.1), the journal (§23.2) and project configuration (08 §8.8). Every check is
// ASCII-exact (D-053): nothing is case-folded, trimmed or Unicode-normalized before comparison.
var (
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	tagSyntax    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestSyntax = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitSyntax = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	decimal      = regexp.MustCompile(`^(0|[1-9][0-9]{0,18})$`)
	workflowPath = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9_-][A-Za-z0-9._-]*\.(yaml|yml)$`)
	timestamp    = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)
	// OCI distribution-spec repository name components, joined by single slashes.
	repoComponent = `[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*`
	repoPath      = regexp.MustCompile(`^` + repoComponent + `(/` + repoComponent + `)*$`)
	hostLabel     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	portSyntax    = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)
	githubOwner   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,38})$`)
	githubRepo    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// Bounds from §23.1.
const (
	MaxImages        = 32
	maxImageBytes    = 512
	maxHostBytes     = 253
	maxRepoPathBytes = 255
	maxRepoURLBytes  = 2048
	maxWorkflowBytes = 255
)

// DNSLabel reports a project name or registry binding name: 1–63 lowercase ASCII bytes.
func DNSLabel(s string) bool { return dnsLabel.MatchString(s) }

// Tag reports OCI tag syntax, at most 128 bytes.
func Tag(s string) bool { return tagSyntax.MatchString(s) }

// Digest reports "sha256:" plus 64 lowercase hex digits.
func Digest(s string) bool { return digestSyntax.MatchString(s) }

// Commit reports a full lowercase 40- or 64-hex commit SHA (D-044).
func Commit(s string) bool { return commitSyntax.MatchString(s) }

// RunNumber reports a canonical decimal string from 1 through 9223372036854775807.
func RunNumber(s string) bool {
	n, ok := Sequence(s)
	return ok && n >= 1
}

// Sequence parses a canonical decimal string from 0 through 9223372036854775807.
func Sequence(s string) (int64, bool) {
	if !decimal.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// Workflow reports ".github/workflows/" plus one .yaml or .yml file name, at most 255 bytes.
func Workflow(s string) bool { return len(s) <= maxWorkflowBytes && workflowPath.MatchString(s) }

// Timestamp parses UTC RFC 3339 seconds, exactly YYYY-MM-DDTHH:MM:SSZ, rejecting impossible dates.
func Timestamp(s string) (time.Time, bool) {
	if !timestamp.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil || t.Format(time.RFC3339) != s {
		return time.Time{}, false
	}
	return t, true
}

// Host reports a registry authority: a lowercase ASCII DNS name, optionally with a port, at most 253 bytes. It has
// no userinfo, path, percent-encoding, trailing dot or IP-literal brackets (§23.6).
func Host(s string) bool {
	if s == "" || len(s) > maxHostBytes {
		return false
	}
	name, port, hasPort := strings.Cut(s, ":")
	if hasPort {
		n, err := strconv.Atoi(port)
		if !portSyntax.MatchString(port) || err != nil || n > 65535 {
			return false
		}
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) > 63 || !hostLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// RepositoryPath reports a lowercase OCI repository path of at most 255 bytes, with no tag, digest or traversal.
func RepositoryPath(s string) bool { return len(s) <= maxRepoPathBytes && repoPath.MatchString(s) }

// SplitImage splits an image name into its registry host and repository path, reporting false unless it is exactly
// host/path under §23.1: at most 512 bytes, with no scheme, credentials, tag or digest.
func SplitImage(s string) (host, path string, ok bool) {
	if len(s) > maxImageBytes {
		return "", "", false
	}
	host, path, found := strings.Cut(s, "/")
	if !found || !Host(host) || !RepositoryPath(path) {
		return "", "", false
	}
	return host, path, true
}

// CanonicalRepo returns the canonical identity of a GitHub repository URL: "https://<host>/<owner>/<repo>", with an
// optional ".git" suffix removed (20 §20.2). It reports false for any other URL shape.
func CanonicalRepo(raw string) (string, bool) {
	if len(raw) > maxRepoURLBytes {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.RawPath != "" || u.Opaque != "" || !Host(u.Host) || strings.Contains(u.Host, ":") {
		return "", false
	}
	owner, repo, found := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	repo = strings.TrimSuffix(repo, ".git")
	if !found || !strings.HasPrefix(u.Path, "/") || !githubOwner.MatchString(owner) || !githubRepo.MatchString(repo) ||
		repo == "." || repo == ".." {
		return "", false
	}
	return "https://" + u.Host + "/" + owner + "/" + repo, true
}

// ReleaseTag is the immutable descriptor tag of a run attempt, "pc-release-<runId>-<runAttempt>" (§23.2). Callers
// pass validated run numbers, as for ReceiptTag and JournalTag.
func ReleaseTag(runID, runAttempt string) string { return "pc-release-" + runID + "-" + runAttempt }

// ReceiptTag is the immutable per-run receipt of a journal entry.
func ReceiptTag(runID, runAttempt string) string { return "pc-publication-" + runID + "-" + runAttempt }

// JournalTag is the immutable tag of the journal entry at sequence.
func JournalTag(sequence int64) string { return "pc-journal-" + strconv.FormatInt(sequence, 10) }

// HeadTag is the single mutable tag of the journal head.
const HeadTag = "pc-release-head"

// ParseDescriptorTag reports the run ID and attempt of a canonical "pc-release-<runId>-<runAttempt>" tag.
func ParseDescriptorTag(tag string) (runID, runAttempt string, ok bool) {
	rest, found := strings.CutPrefix(tag, "pc-release-")
	if !found {
		return "", "", false
	}
	runID, runAttempt, found = strings.Cut(rest, "-")
	if !found || !RunNumber(runID) || !RunNumber(runAttempt) {
		return "", "", false
	}
	return runID, runAttempt, true
}
