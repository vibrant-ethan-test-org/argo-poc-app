package publish

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Vibrant-America/promotion-controller/internal/descriptor"
	"github.com/Vibrant-America/promotion-controller/internal/pipeline"
)

// Limits on action inputs (§23.4).
const (
	maxPipelineBytes     = 1 << 20
	maxBuildOutputsBytes = 64 << 10
	maxCredentialBytes   = 16 << 10
	maxPathBytes         = 4096
	maxContextBytes      = 1024
	buildOutputsDepth    = 8
	buildOutputsNodes    = 2048
	clockSkew            = 5 * time.Minute
)

// Context is the trusted GitHub job context: the runner sets these, and the action reads them from the `github`
// context, never from a caller's `with:` inputs (§23.4, §23.5).
type Context struct {
	Actions     string // GITHUB_ACTIONS: "true" on a runner
	ServerURL   string // GITHUB_SERVER_URL
	Repository  string // GITHUB_REPOSITORY: owner/repo
	SHA         string // GITHUB_SHA
	Ref         string // GITHUB_REF
	RefType     string // GITHUB_REF_TYPE
	EventName   string // GITHUB_EVENT_NAME
	WorkflowRef string // GITHUB_WORKFLOW_REF: owner/repo/.github/workflows/<file>@<ref>
	RunID       string // GITHUB_RUN_ID
	RunAttempt  string // GITHUB_RUN_ATTEMPT
	Workspace   string // GITHUB_WORKSPACE: the checkout
	Actor       string // GITHUB_ACTOR: the registry username for the job token
}

// Run is a validated, trusted run identity.
type Run struct {
	Repo       string // canonical https://<host>/<owner>/<repo>
	Repository string // owner/repo
	Commit     string
	Branch     string
	Workflow   string
	RunID      string
	RunAttempt string
}

// trustedEvents may publish. Pull-request events of every kind, and anything else, are refused before any file is
// read: PR jobs, fork jobs and workflows executing PR content never publish (§23.5).
var trustedEvents = map[string]bool{"push": true, "workflow_dispatch": true, "schedule": true}

var ownerRepo = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// trust validates the job context and derives the run identity. The workflow must be defined on the ref that runs it,
// and that ref must be the configured branch.
func (c Context) trust() (Run, error) {
	for _, v := range []string{c.ServerURL, c.Repository, c.SHA, c.Ref, c.RefType, c.EventName, c.WorkflowRef, c.RunID,
		c.RunAttempt, c.Workspace, c.Actor} {
		if len(v) > maxContextBytes {
			return Run{}, fail(CodeContextUntrusted, "")
		}
	}
	switch {
	case c.Actions != "true":
		return Run{}, fail(CodeContextUntrusted, "GITHUB_ACTIONS")
	case !trustedEvents[c.EventName]:
		return Run{}, fail(CodeContextUntrusted, "GITHUB_EVENT_NAME")
	case !ownerRepo.MatchString(c.Repository) || strings.HasSuffix(c.Repository, "."):
		return Run{}, fail(CodeContextUntrusted, "GITHUB_REPOSITORY")
	case !descriptor.Commit(c.SHA):
		return Run{}, fail(CodeContextUntrusted, "GITHUB_SHA")
	case c.RefType != "branch" || !strings.HasPrefix(c.Ref, "refs/heads/") || len(c.Ref) == len("refs/heads/"):
		return Run{}, fail(CodeContextUntrusted, "GITHUB_REF")
	case !descriptor.RunNumber(c.RunID):
		return Run{}, fail(CodeContextUntrusted, "GITHUB_RUN_ID")
	case !descriptor.RunNumber(c.RunAttempt):
		return Run{}, fail(CodeContextUntrusted, "GITHUB_RUN_ATTEMPT")
	}
	repo, ok := descriptor.CanonicalRepo(strings.TrimRight(c.ServerURL, "/") + "/" + c.Repository)
	if !ok || repo != strings.TrimRight(c.ServerURL, "/")+"/"+c.Repository {
		return Run{}, fail(CodeContextUntrusted, "GITHUB_SERVER_URL")
	}
	workflow, ok := strings.CutPrefix(c.WorkflowRef, c.Repository+"/")
	if !ok {
		return Run{}, fail(CodeContextUntrusted, "GITHUB_WORKFLOW_REF")
	}
	workflow, ref, ok := strings.Cut(workflow, "@")
	if !ok || ref != c.Ref || !descriptor.Workflow(workflow) {
		return Run{}, fail(CodeContextUntrusted, "GITHUB_WORKFLOW_REF")
	}
	return Run{
		Repo:       repo,
		Repository: c.Repository,
		Commit:     c.SHA,
		Branch:     strings.TrimPrefix(c.Ref, "refs/heads/"),
		Workflow:   workflow,
		RunID:      c.RunID,
		RunAttempt: c.RunAttempt,
	}, nil
}

// Target is the publication configuration resolved from the pipeline: the project, its watched images and their
// tag patterns, and the descriptor repository on the images' registry host.
type Target struct {
	Project    string
	Repo       string // canonical
	Branch     string
	Host       string
	Repository string // descriptor repository path
	Binding    string // descriptor registry binding name
	Images     []string
	Expect     descriptor.Expectation
}

// loadTarget decodes and validates the pipeline with the controller's strict decoder and shared source validator
// (08 §8.2, §8.8). CI cannot see the platform, so descriptor mode also requires what the platform could otherwise
// change: an explicit tagPattern on every image, and no expansion syntax in source values.
func loadTarget(data []byte, githubWeb string) (Target, error) {
	p, err := pipeline.Decode(data)
	if err != nil {
		return Target{}, failWith(CodePipelineInvalid, pathOf(err), err)
	}
	if errs := pipeline.ValidateSource(p.Source, githubWeb); len(errs) > 0 {
		return Target{}, failWith(CodePipelineInvalid, pathOf(errs[0]), errs[0])
	}
	if !descriptor.DNSLabel(p.Project) {
		return Target{}, fail(CodePipelineInvalid, "project")
	}
	src := p.Source
	if src.Descriptor == nil {
		return Target{}, fail(CodePipelineInvalid, "source.descriptor")
	}
	values := []string{src.Repo, src.Branch, src.Descriptor.Registry, src.Descriptor.Repository}
	for _, img := range src.Images {
		values = append(values, img.Image, img.Registry, img.TagPattern)
	}
	for _, v := range values {
		if strings.Contains(v, "${") || strings.Contains(v, "$$") {
			return Target{}, fail(CodePipelineInvalid, "source")
		}
	}
	repo, ok := descriptor.CanonicalRepo(src.Repo)
	if !ok {
		return Target{}, fail(CodePipelineInvalid, "source.repo")
	}
	t := Target{
		Project:    p.Project,
		Repo:       repo,
		Branch:     src.Branch,
		Repository: src.Descriptor.Repository,
		Binding:    src.Descriptor.Registry,
		Expect:     descriptor.Expectation{Project: p.Project, Repo: repo, Images: map[string]*regexp.Regexp{}},
	}
	for i, img := range src.Images {
		if img.TagPattern == "" {
			return Target{}, fail(CodePipelineInvalid, fmt.Sprintf("source.images[%d].tagPattern", i))
		}
		pattern, err := regexp.Compile(img.TagPattern)
		if err != nil {
			return Target{}, fail(CodePipelineInvalid, fmt.Sprintf("source.images[%d].tagPattern", i))
		}
		host, _, _ := descriptor.SplitImage(img.Image) // ValidateSource checked the syntax and the single host
		t.Host = host
		t.Images = append(t.Images, img.Image)
		t.Expect.Images[img.Image] = pattern
	}
	return t, nil
}

// pathOf returns a pipeline error's schema path, which echoes nothing of the document.
func pathOf(err error) string {
	var pe *pipeline.PathError
	if errors.As(err, &pe) && len(pe.Path) <= 256 {
		return pe.Path
	}
	return ""
}

// BuildOutputs is the action's build-output file (§23.4): the common run identity, the explicit display version and
// exactly one entry per watched image.
type BuildOutputs struct {
	Run     Run
	Version string
	Images  []BuildImage
}

// BuildImage is one image job's output.
type BuildImage struct {
	Image       string
	Tag         string
	Digest      string
	Commit      string
	CompletedAt string
	Result      string
}

// parseBuildOutputs strictly decodes the build-output file: at most 64 KiB, 8 levels and 2,048 nodes, no unknown,
// duplicate or null fields and no trailing JSON. Errors carry schema paths only.
func parseBuildOutputs(data []byte) (BuildOutputs, error) {
	bad := func(path string) (BuildOutputs, error) { return BuildOutputs{}, fail(CodeBuildOutputsInvalid, path) }
	v, err := descriptor.Parse(data, descriptor.Limits{Bytes: maxBuildOutputsBytes, Depth: buildOutputsDepth, Nodes: buildOutputsNodes})
	if err != nil {
		return bad("")
	}
	top, ok := object(v, "schemaVersion", "run", "version", "images")
	if !ok || top["schemaVersion"].Kind != descriptor.Number || top["schemaVersion"].Int != 1 {
		return bad("")
	}
	run, ok := object(top["run"], "repository", "commit", "workflow", "runId", "runAttempt")
	if !ok || !allStrings(run) {
		return bad("run")
	}
	out := BuildOutputs{Run: Run{
		Repository: run["repository"].Str,
		Commit:     run["commit"].Str,
		Workflow:   run["workflow"].Str,
		RunID:      run["runId"].Str,
		RunAttempt: run["runAttempt"].Str,
	}}
	if top["version"].Kind != descriptor.String {
		return bad("version")
	}
	out.Version = top["version"].Str
	images := top["images"]
	if images.Kind != descriptor.Array || len(images.Items) == 0 || len(images.Items) > descriptor.MaxImages {
		return bad("images")
	}
	for i, item := range images.Items {
		img, ok := object(item, "image", "tag", "digest", "commit", "completedAt", "result")
		if !ok || !allStrings(img) {
			return bad(fmt.Sprintf("images[%d]", i))
		}
		out.Images = append(out.Images, BuildImage{
			Image:       img["image"].Str,
			Tag:         img["tag"].Str,
			Digest:      img["digest"].Str,
			Commit:      img["commit"].Str,
			CompletedAt: img["completedAt"].Str,
			Result:      img["result"].Str,
		})
	}
	return out, nil
}

func object(v descriptor.Value, keys ...string) (map[string]descriptor.Value, bool) {
	if v.Kind != descriptor.Object || len(v.Members) != len(keys) {
		return nil, false
	}
	out := map[string]descriptor.Value{}
	for _, m := range v.Members {
		out[m.Key] = m.Value
	}
	for _, k := range keys {
		if _, ok := out[k]; !ok {
			return nil, false
		}
	}
	return out, true
}

func allStrings(m map[string]descriptor.Value) bool {
	for _, v := range m {
		if v.Kind != descriptor.String {
			return false
		}
	}
	return true
}

// release checks the build outputs against the trusted run and the configured target and returns the release
// payload. Cross-run outputs, failed or missing image jobs, extra or duplicate images and impossible or future
// completion times are refused; completedAt is the latest image completion of this run (§23.1).
func (o BuildOutputs) release(run Run, t Target, now time.Time) (descriptor.Release, error) {
	if o.Run.Repository != run.Repository || o.Run.Commit != run.Commit || o.Run.Workflow != run.Workflow ||
		o.Run.RunID != run.RunID || o.Run.RunAttempt != run.RunAttempt {
		return descriptor.Release{}, fail(CodeRunMismatch, "run")
	}
	if len(o.Version) > 128 || !descriptor.Tag(o.Version) {
		return descriptor.Release{}, fail(CodeBuildOutputsInvalid, "version")
	}
	r := descriptor.Release{
		Project:    t.Project,
		Repo:       run.Repo,
		Commit:     run.Commit,
		Workflow:   run.Workflow,
		RunID:      run.RunID,
		RunAttempt: run.RunAttempt,
		Version:    o.Version,
	}
	seen := map[string]bool{}
	var latest time.Time
	for i, img := range o.Images {
		path := fmt.Sprintf("images[%d]", i)
		if img.Result != "success" {
			return descriptor.Release{}, fail(CodeBuildIncomplete, path+".result")
		}
		if img.Commit != run.Commit {
			return descriptor.Release{}, fail(CodeRunMismatch, path+".commit")
		}
		if _, ok := t.Expect.Images[img.Image]; !ok || seen[img.Image] {
			return descriptor.Release{}, fail(CodeImageSetMismatch, path+".image")
		}
		seen[img.Image] = true
		if len(img.Tag) > 128 || !descriptor.Tag(img.Tag) {
			return descriptor.Release{}, fail(CodeBuildOutputsInvalid, path+".tag")
		}
		if !t.Expect.Images[img.Image].MatchString(img.Tag) {
			return descriptor.Release{}, fail(CodeTagPatternMismatch, path+".tag")
		}
		if !descriptor.Digest(img.Digest) {
			return descriptor.Release{}, fail(CodeBuildOutputsInvalid, path+".digest")
		}
		done, ok := descriptor.Timestamp(img.CompletedAt)
		if !ok || done.After(now.Add(clockSkew)) {
			return descriptor.Release{}, fail(CodeBuildOutputsInvalid, path+".completedAt")
		}
		if done.After(latest) {
			latest = done
			r.CompletedAt = img.CompletedAt
		}
		r.Images = append(r.Images, descriptor.Image{Image: img.Image, Tag: img.Tag, Digest: img.Digest})
	}
	if len(seen) != len(t.Expect.Images) {
		return descriptor.Release{}, fail(CodeBuildIncomplete, "images")
	}
	r = r.Sorted()
	if err := r.Check(t.Expect); err != nil {
		return descriptor.Release{}, failWith(CodeBuildOutputsInvalid, "", err)
	}
	return r, nil
}
