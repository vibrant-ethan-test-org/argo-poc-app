package descriptor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
)

// Media types of the release descriptor (§23.1) and its journal (§23.2).
const (
	MediaTypeManifest   = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeIndex      = "application/vnd.oci.image.index.v1+json"
	MediaTypeEmpty      = "application/vnd.oci.empty.v1+json"
	MediaTypeRelease    = "application/vnd.promotion.release.v1+json"
	MediaTypeHead       = "application/vnd.promotion.release-head.v1+json"
	MediaTypeEntry      = "application/vnd.promotion.release-entry.v1+json"
	AnnotationCreated   = "org.opencontainers.image.created"
	AnnotationRevision  = "org.opencontainers.image.revision"
	ProviderGitHub      = "github"
	releaseSchema       = 1
	maxReleaseBytes     = 64 << 10
	maxReleaseDepth     = 8
	maxReleaseNodes     = 2048
	maxReleaseVersion   = 128
	maxReleaseTagLength = 128
)

// EmptyConfig is OCI's empty config blob; its descriptor carries EmptyConfigDigest and size 2 (§23.1).
var EmptyConfig = []byte("{}")

// EmptyConfigDigest is the SHA-256 of EmptyConfig.
const EmptyConfigDigest = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"

// Error is a fixed validation failure. Code is one of the fixed codes of §23.4 and Path a schema path made of field
// names and indices; neither ever contains input.
type Error struct {
	Code string
	Path string
}

func (e *Error) Error() string {
	if e.Path == "" {
		return e.Code
	}
	return e.Code + " at " + e.Path
}

// Fixed validation codes (§23.4).
const (
	CodePayloadInvalid  = "payloadInvalid"
	CodePayloadTooLarge = "payloadTooLarge"
	CodeNotCanonical    = "payloadNotCanonical"
	CodeUnsupported     = "schemaVersionUnsupported"
	CodeProjectMismatch = "projectMismatch"
	CodeRepoMismatch    = "repositoryMismatch"
	CodeImageSet        = "imageSetMismatch"
	CodeTagPattern      = "tagPatternMismatch"
	CodeManifestInvalid = "manifestInvalid"
	CodeJournalInvalid  = "journalInvalid"
)

func invalid(code, path string) error { return &Error{Code: code, Path: path} }

// Release is the v1 release payload (§23.1).
type Release struct {
	Project     string
	Repo        string
	Commit      string
	Workflow    string
	RunID       string
	RunAttempt  string
	CompletedAt string
	Version     string
	Images      []Image
}

// Image is one built image of a release.
type Image struct {
	Image  string
	Tag    string
	Digest string
}

// Expectation is what a release must match from its project's configuration.
type Expectation struct {
	Project string
	Repo    string                    // canonical (CanonicalRepo)
	Images  map[string]*regexp.Regexp // configured image name → compiled tagPattern
}

// Sorted returns a copy of r with its images sorted by byte-exact image name, the canonical order (§23.1).
func (r Release) Sorted() Release {
	r.Images = append([]Image(nil), r.Images...)
	sort.Slice(r.Images, func(i, j int) bool { return r.Images[i].Image < r.Images[j].Image })
	return r
}

// Validate checks every syntax bound of §23.1 that needs no configuration.
func (r Release) Validate() error {
	switch {
	case !DNSLabel(r.Project):
		return invalid(CodePayloadInvalid, "project")
	case !validRepo(r.Repo):
		return invalid(CodePayloadInvalid, "source.repo")
	case !Commit(r.Commit):
		return invalid(CodePayloadInvalid, "source.commit")
	case !Workflow(r.Workflow):
		return invalid(CodePayloadInvalid, "build.workflow")
	case !RunNumber(r.RunID):
		return invalid(CodePayloadInvalid, "build.runId")
	case !RunNumber(r.RunAttempt):
		return invalid(CodePayloadInvalid, "build.runAttempt")
	case !validTimestamp(r.CompletedAt):
		return invalid(CodePayloadInvalid, "build.completedAt")
	case len(r.Version) > maxReleaseVersion || !Tag(r.Version):
		return invalid(CodePayloadInvalid, "version")
	case len(r.Images) == 0 || len(r.Images) > MaxImages:
		return invalid(CodePayloadInvalid, "images")
	}
	seen := map[string]bool{}
	for i, img := range r.Images {
		path := fmt.Sprintf("images[%d]", i)
		if _, _, ok := SplitImage(img.Image); !ok {
			return invalid(CodePayloadInvalid, path+".image")
		}
		if seen[img.Image] {
			return invalid(CodeImageSet, path+".image")
		}
		seen[img.Image] = true
		if len(img.Tag) > maxReleaseTagLength || !Tag(img.Tag) {
			return invalid(CodePayloadInvalid, path+".tag")
		}
		if !Digest(img.Digest) {
			return invalid(CodePayloadInvalid, path+".digest")
		}
	}
	return nil
}

func validRepo(s string) bool {
	c, ok := CanonicalRepo(s)
	return ok && c == s
}

func validTimestamp(s string) bool {
	_, ok := Timestamp(s)
	return ok
}

// Check validates r and matches it against its project's configuration: the same project and canonical repository,
// exactly the configured image-name set, and every tag matching its image's tagPattern.
func (r Release) Check(exp Expectation) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Project != exp.Project {
		return invalid(CodeProjectMismatch, "project")
	}
	if r.Repo != exp.Repo {
		return invalid(CodeRepoMismatch, "source.repo")
	}
	if len(r.Images) != len(exp.Images) {
		return invalid(CodeImageSet, "images")
	}
	for i, img := range r.Images {
		pattern, ok := exp.Images[img.Image]
		if !ok {
			return invalid(CodeImageSet, fmt.Sprintf("images[%d].image", i))
		}
		if pattern == nil || !pattern.MatchString(img.Tag) {
			return invalid(CodeTagPattern, fmt.Sprintf("images[%d].tag", i))
		}
	}
	return nil
}

// Encode returns the canonical payload bytes of r: RFC 8785 JSON, UTF-8, no BOM or trailing newline, with images
// sorted by image name. It validates syntax first, so it never encodes an invalid release.
func (r Release) Encode() ([]byte, error) {
	r = r.Sorted()
	if err := r.Validate(); err != nil {
		return nil, err
	}
	images := make([]Value, len(r.Images))
	for i, img := range r.Images {
		images[i] = Obj(Member{"image", Str(img.Image)}, Member{"tag", Str(img.Tag)}, Member{"digest", Str(img.Digest)})
	}
	doc := Obj(
		Member{"schemaVersion", Int(releaseSchema)},
		Member{"project", Str(r.Project)},
		Member{"source", Obj(Member{"repo", Str(r.Repo)}, Member{"commit", Str(r.Commit)})},
		Member{"build", Obj(
			Member{"provider", Str(ProviderGitHub)},
			Member{"workflow", Str(r.Workflow)},
			Member{"runId", Str(r.RunID)},
			Member{"runAttempt", Str(r.RunAttempt)},
			Member{"completedAt", Str(r.CompletedAt)},
		)},
		Member{"version", Str(r.Version)},
		Member{"images", Arr(images...)},
	)
	return Canonical(doc), nil
}

// ParseRelease strictly decodes canonical payload bytes. It enforces the size, depth and node limits before and while
// decoding, rejects unknown, duplicate or null fields, a schema version other than 1 and trailing JSON, validates
// every field and requires the bytes to equal their canonical encoding, including the image order.
func ParseRelease(data []byte) (Release, error) {
	if len(data) > maxReleaseBytes {
		return Release{}, invalid(CodePayloadTooLarge, "")
	}
	v, err := Parse(data, Limits{Bytes: maxReleaseBytes, Depth: maxReleaseDepth, Nodes: maxReleaseNodes})
	if err != nil {
		return Release{}, invalid(CodePayloadInvalid, "")
	}
	top, ok := fields(v, []string{"schemaVersion", "project", "source", "build", "version", "images"})
	if !ok {
		// A document that names another schema version is unsupported, not merely malformed.
		if sv, found := member(v, "schemaVersion"); found && (sv.Kind != Number || sv.Int != releaseSchema) {
			return Release{}, invalid(CodeUnsupported, "schemaVersion")
		}
		return Release{}, invalid(CodePayloadInvalid, "")
	}
	if sv := top["schemaVersion"]; sv.Kind != Number || sv.Int != releaseSchema {
		return Release{}, invalid(CodeUnsupported, "schemaVersion")
	}
	var r Release
	var problem string
	str := func(v Value, path string) string {
		if v.Kind != String && problem == "" {
			problem = path
		}
		return v.Str
	}
	r.Project = str(top["project"], "project")
	r.Version = str(top["version"], "version")
	src, ok := fields(top["source"], []string{"repo", "commit"})
	if !ok {
		return Release{}, invalid(CodePayloadInvalid, "source")
	}
	r.Repo = str(src["repo"], "source.repo")
	r.Commit = str(src["commit"], "source.commit")
	build, ok := fields(top["build"], []string{"provider", "workflow", "runId", "runAttempt", "completedAt"})
	if !ok {
		return Release{}, invalid(CodePayloadInvalid, "build")
	}
	if str(build["provider"], "build.provider") != ProviderGitHub && problem == "" {
		problem = "build.provider"
	}
	r.Workflow = str(build["workflow"], "build.workflow")
	r.RunID = str(build["runId"], "build.runId")
	r.RunAttempt = str(build["runAttempt"], "build.runAttempt")
	r.CompletedAt = str(build["completedAt"], "build.completedAt")
	images := top["images"]
	if images.Kind != Array {
		return Release{}, invalid(CodePayloadInvalid, "images")
	}
	for i, item := range images.Items {
		path := fmt.Sprintf("images[%d]", i)
		img, ok := fields(item, []string{"image", "tag", "digest"})
		if !ok {
			return Release{}, invalid(CodePayloadInvalid, path)
		}
		r.Images = append(r.Images, Image{
			Image:  str(img["image"], path+".image"),
			Tag:    str(img["tag"], path+".tag"),
			Digest: str(img["digest"], path+".digest"),
		})
	}
	if problem != "" {
		return Release{}, invalid(CodePayloadInvalid, problem)
	}
	if err := r.Validate(); err != nil {
		return Release{}, err
	}
	canonical, err := r.Encode()
	if err != nil {
		return Release{}, err
	}
	if !bytes.Equal(canonical, data) {
		return Release{}, invalid(CodeNotCanonical, "")
	}
	return r, nil
}

func member(v Value, key string) (Value, bool) {
	if v.Kind != Object {
		return Value{}, false
	}
	for _, m := range v.Members {
		if m.Key == key {
			return m.Value, true
		}
	}
	return Value{}, false
}

// SHA256 returns the "sha256:<hex>" digest of b.
func SHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
