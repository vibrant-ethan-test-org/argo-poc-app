// Package publish is the release-descriptor publisher behind the shared GitHub Action (docs/design/23-release-
// descriptors.md §23.2, §23.4, §23.5; D-072). It validates a trusted run's build outputs against the project's
// pipeline, publishes the canonical descriptor and its per-image referrers, and commits the publication by appending
// to the descriptor repository's journal, writing the head last.
package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Vibrant-America/promotion-controller/internal/descriptor"
)

// maxVerifyLinks bounds the journal links a committed retry walks back to prove its entry is still reachable from
// an advanced head, matching the foreground discovery budget (§23.6).
const maxVerifyLinks = 1000

// Options are the action's inputs (§23.4) and the trusted job context.
type Options struct {
	Context              Context
	PipelineFile         string // repository-relative; default deploy/pipeline.yaml
	BuildOutputsFile     string
	DescriptorRegistry   string // optional; must equal the pipeline's binding name
	DescriptorRepository string // optional; must equal the pipeline's repository
	CredentialFile       string // optional regular file holding the registry password or token
	JobToken             string // the job's short-lived token, used when CredentialFile is empty
	InitializeJournal    bool   // explicitly write sequence 0 when the head is missing and no entry exists
	StateDir             string // where the prepared-publication record is kept
	OutputFile           string // GITHUB_OUTPUT
}

// Deps are the publisher's collaborators; tests replace them.
type Deps struct {
	Source       SourceReader
	HTTP         *http.Client
	RegistryBase func(host string) string // the registry origin for a host; https://<host> by default
	Now          func() time.Time
	Sleep        func(context.Context, time.Duration) error
	Log          io.Writer
}

// Result is a verified publication.
type Result struct {
	DescriptorRef    string
	DescriptorDigest string
	PayloadDigest    string
	PublishedAt      string
	Sequence         int64
	EntryDigest      string
	Duplicate        bool // this invocation found the publication already committed and appended nothing
}

// Outputs returns the action outputs of a verified publication.
func (r Result) Outputs() []Output {
	return []Output{
		{"descriptor-ref", r.DescriptorRef},
		{"descriptor-digest", r.DescriptorDigest},
		{"payload-digest", r.PayloadDigest},
		{"published-at", r.PublishedAt},
		{"journal-sequence", strconv.FormatInt(r.Sequence, 10)},
		{"journal-entry-digest", r.EntryDigest},
		{"duplicate-publication", strconv.FormatBool(r.Duplicate)},
	}
}

type publisher struct {
	reg    *Registry
	target Target
	run    Run
	opts   Options
	deps   Deps
	state  prepared
	// descriptor is the canonical manifest and copies the referrer manifests, by image name.
	descriptor descriptor.Manifest
	copies     map[string]descriptor.Manifest
}

func (p *publisher) now() time.Time {
	if p.deps.Now != nil {
		return p.deps.Now().UTC()
	}
	return time.Now().UTC()
}

func (p *publisher) logf(format string, args ...string) {
	if p.deps.Log == nil {
		return
	}
	escaped := make([]any, len(args))
	for i, a := range args {
		escaped[i] = EscapeLog(a)
	}
	_, _ = fmt.Fprintf(p.deps.Log, format+"\n", escaped...) // progress only; failures are reported by the result
}

// Publish runs the whole publication and, only after it is verified, writes the outputs.
func Publish(ctx context.Context, opts Options, deps Deps) (Result, error) {
	p := &publisher{opts: opts, deps: deps, copies: map[string]descriptor.Manifest{}}
	res, err := p.publish(ctx)
	if err != nil {
		return Result{}, err
	}
	if err := writeOutputs(opts.OutputFile, res.Outputs()); err != nil {
		return Result{}, err
	}
	return res, nil
}

func (p *publisher) publish(ctx context.Context) (Result, error) {
	release, err := p.validate(ctx)
	if err != nil {
		return Result{}, err
	}
	subjects := map[string]descriptor.Descriptor{}
	for i, img := range release.Images {
		if subjects[img.Image], err = p.inspectImage(ctx, i, img, release.Commit); err != nil {
			return Result{}, err
		}
	}
	// Scoped write-access preflight before any content is uploaded (§23.2 step 1).
	for _, repo := range p.repositories(release) {
		if err := p.reg.ProbeWrite(ctx, repo); err != nil {
			return Result{}, registryFailure(err, "write preflight")
		}
	}
	if err := p.prepare(ctx, release, subjects); err != nil {
		return Result{}, err
	}
	head, err := p.readHead(ctx)
	if err != nil {
		return Result{}, err
	}
	if res, done, err := p.committed(ctx, head); err != nil || done {
		return res, err
	}
	if err := p.upload(ctx, release); err != nil {
		return Result{}, err
	}
	return p.append(ctx, head)
}

func (p *publisher) repositories(r descriptor.Release) []string {
	repos := []string{p.target.Repository}
	for _, img := range r.Images {
		_, repo, _ := descriptor.SplitImage(img.Image)
		repos = append(repos, repo)
	}
	return repos
}

// validate checks the trusted context, the pipeline at the exact source commit and the build outputs, and returns the
// release payload. It reads nothing from a registry.
func (p *publisher) validate(ctx context.Context) (descriptor.Release, error) {
	run, err := p.opts.Context.trust()
	if err != nil {
		return descriptor.Release{}, err
	}
	p.run = run
	path := p.opts.PipelineFile
	if path == "" {
		path = "deploy/pipeline.yaml"
	}
	if !validPipelinePath(path) {
		return descriptor.Release{}, fail(CodeInputInvalid, "pipeline-file")
	}
	if !filepath.IsAbs(p.opts.Context.Workspace) {
		return descriptor.Release{}, fail(CodeContextUntrusted, "GITHUB_WORKSPACE")
	}
	data, err := p.deps.Source.Pipeline(ctx, p.opts.Context.Workspace, run.Commit, path)
	if err != nil {
		var f *Failure
		if errors.As(err, &f) {
			return descriptor.Release{}, err
		}
		return descriptor.Release{}, failWith(CodePipelineMismatch, "pipeline-file", err)
	}
	if p.target, err = loadTarget(data, strings.TrimRight(p.opts.Context.ServerURL, "/")); err != nil {
		return descriptor.Release{}, err
	}
	if p.target.Repo != run.Repo {
		return descriptor.Release{}, fail(CodeRunMismatch, "source.repo")
	}
	if p.target.Branch != run.Branch {
		return descriptor.Release{}, fail(CodeBranchNotConfigured, "source.branch")
	}
	if p.opts.DescriptorRegistry != "" && p.opts.DescriptorRegistry != p.target.Binding {
		return descriptor.Release{}, fail(CodeRegistryOverride, "descriptor-registry")
	}
	if p.opts.DescriptorRepository != "" && p.opts.DescriptorRepository != p.target.Repository {
		return descriptor.Release{}, fail(CodeRegistryOverride, "descriptor-repository")
	}
	outputsPath := p.opts.BuildOutputsFile
	if len(outputsPath) > maxPathBytes || outputsPath == "" {
		return descriptor.Release{}, fail(CodeInputInvalid, "build-outputs-file")
	}
	if !filepath.IsAbs(outputsPath) {
		outputsPath = filepath.Join(p.opts.Context.Workspace, outputsPath)
	}
	raw, err := readRegular(outputsPath, maxBuildOutputsBytes, "build-outputs-file")
	if err != nil {
		return descriptor.Release{}, err
	}
	outputs, err := parseBuildOutputs(raw)
	if err != nil {
		return descriptor.Release{}, err
	}
	release, err := outputs.release(run, p.target, p.now())
	if err != nil {
		return descriptor.Release{}, err
	}
	password := p.opts.JobToken
	if p.opts.CredentialFile != "" {
		if len(p.opts.CredentialFile) > maxPathBytes || !filepath.IsAbs(p.opts.CredentialFile) {
			return descriptor.Release{}, fail(CodeInputInvalid, "credential-file")
		}
		secret, err := readRegular(p.opts.CredentialFile, maxCredentialBytes, "credential-file")
		if err != nil {
			return descriptor.Release{}, fail(CodeCredentialInvalid, "credential-file")
		}
		password = strings.TrimSuffix(strings.TrimSuffix(string(secret), "\n"), "\r")
	}
	if len(password) > maxCredentialBytes || strings.ContainsAny(password, "\r\n\x00") {
		return descriptor.Release{}, fail(CodeCredentialInvalid, "")
	}
	base := "https://" + p.target.Host
	if p.deps.RegistryBase != nil {
		base = p.deps.RegistryBase(p.target.Host)
	}
	p.reg = &Registry{Base: base, Username: p.opts.Context.Actor, Password: password, HTTP: p.deps.HTTP, Sleep: p.deps.Sleep}
	if p.reg.Username == "" {
		p.reg.Username = "release-descriptor"
	}
	p.logf("release-descriptor: validated %s run %s attempt %s", p.target.Project, run.RunID, run.RunAttempt)
	return release, nil
}

func (p *publisher) descriptorTag() string {
	return descriptor.ReleaseTag(p.run.RunID, p.run.RunAttempt)
}

func (p *publisher) receiptTag() string { return descriptor.ReceiptTag(p.run.RunID, p.run.RunAttempt) }

// prepare builds or reloads the prepared-publication record (§23.2 step 2). Without a record, an existing descriptor
// tag of this run with the exact payload is adopted with its timestamp; one with other bytes is a conflict.
func (p *publisher) prepare(ctx context.Context, release descriptor.Release, subjects map[string]descriptor.Descriptor) error {
	payload, err := release.Encode()
	if err != nil {
		return failWith(CodeBuildOutputsInvalid, "", err)
	}
	state, found, err := loadState(p.opts.StateDir, p.run.RunID, p.run.RunAttempt)
	if err != nil {
		return err
	}
	if found {
		if !bytesEqual(state.Payload, payload) {
			return fail(CodePublicationConflict, "payload")
		}
	} else {
		state = prepared{RunID: p.run.RunID, RunAttempt: p.run.RunAttempt, Payload: payload}
		existing, err := p.reg.GetManifest(ctx, p.target.Repository, p.descriptorTag())
		switch {
		case errors.Is(err, errNotFound):
			// publicationStartedAt is captured just before the first publication request.
			state.StartedAt = p.now().Format(time.RFC3339)
		case err != nil:
			return registryFailure(err, "descriptor tag")
		default:
			m, err := descriptor.ParseManifest(existing.Body, descriptor.MediaTypeRelease)
			if err != nil || m.Subject != nil || m.Created == "" ||
				m.Layer != descriptor.ReleaseManifest(payload, m.Created, nil).Layer {
				return fail(CodePublicationConflict, "descriptor tag")
			}
			state.StartedAt = m.Created
		}
	}
	p.descriptor = descriptor.ReleaseManifest(payload, state.StartedAt, nil)
	if found && !bytesEqual(state.Manifest, p.descriptor.Encode()) {
		return fail(CodeStateInvalid, "manifest")
	}
	state.Manifest = p.descriptor.Encode()
	var copies []copyRecord
	for _, img := range release.Images {
		subject := subjects[img.Image]
		m := descriptor.ReleaseManifest(payload, state.StartedAt, &subject)
		p.copies[img.Image] = m
		copies = append(copies, copyRecord{Image: img.Image, Manifest: m.Encode()})
	}
	if found {
		if len(state.Copies) != len(copies) {
			return fail(CodeStateInvalid, "copies")
		}
		for i := range copies {
			if state.Copies[i].Image != copies[i].Image || !bytesEqual(state.Copies[i].Manifest, copies[i].Manifest) {
				return fail(CodeStateInvalid, "copies")
			}
		}
	}
	state.Copies = copies
	p.state = state
	return p.state.save(p.opts.StateDir)
}

// headRead is a verified journal head and the manifest digest it was read with.
type headRead struct {
	head   descriptor.Head
	digest string
}

// readHead reads and verifies the journal head (§23.2). A missing head is unknown, never an empty journal; it is
// initialized at sequence 0 only when explicitly asked and no first entry exists.
func (p *publisher) readHead(ctx context.Context) (headRead, error) {
	h, err := p.fetchHead(ctx)
	if !errors.Is(err, errNotFound) {
		if err == nil && h.head.Sequence > 0 {
			if _, err := p.entryAt(ctx, h.head.Sequence, h.head.EntryDigest); err != nil {
				return headRead{}, err
			}
		}
		return h, err
	}
	if !p.opts.InitializeJournal {
		return headRead{}, fail(CodeJournalHeadMissing, "")
	}
	if _, err := p.reg.GetManifest(ctx, p.target.Repository, descriptor.JournalTag(1)); !errors.Is(err, errNotFound) {
		if err != nil {
			return headRead{}, registryFailure(err, "journal")
		}
		return headRead{}, fail(CodeIndexConflict, "journal without head")
	}
	zero := descriptor.Head{Project: p.target.Project}
	if err := p.writeHead(ctx, zero, nil); err != nil {
		return headRead{}, err
	}
	p.logf("release-descriptor: initialized journal head at sequence 0")
	return p.fetchHead(ctx)
}

// fetchHead reads the head artifact: manifest, then its payload by digest.
func (p *publisher) fetchHead(ctx context.Context) (headRead, error) {
	m, err := p.reg.GetManifest(ctx, p.target.Repository, descriptor.HeadTag)
	if errors.Is(err, errNotFound) {
		return headRead{}, errNotFound
	}
	if err != nil {
		return headRead{}, registryFailure(err, "head")
	}
	manifest, err := descriptor.ParseManifest(m.Body, descriptor.MediaTypeHead)
	if err != nil || manifest.Created != "" || manifest.Subject != nil {
		return headRead{}, fail(CodeIndexConflict, "head")
	}
	payload, err := p.blob(ctx, manifest.Layer)
	if err != nil {
		return headRead{}, err
	}
	head, err := descriptor.ParseHead(payload, p.target.Project)
	if err != nil {
		return headRead{}, failWith(CodeIndexConflict, "head", err)
	}
	return headRead{head: head, digest: m.Digest}, nil
}

func (p *publisher) blob(ctx context.Context, d descriptor.Descriptor) ([]byte, error) {
	if d.Size > 64<<10 {
		return nil, fail(CodeIndexConflict, "payload size")
	}
	data, err := p.reg.GetBlob(ctx, p.target.Repository, d.Digest, 64<<10)
	if errors.Is(err, errNotFound) {
		return nil, fail(CodeIndexConflict, "payload")
	}
	if err != nil {
		return nil, registryFailure(err, "payload")
	}
	if int64(len(data)) != d.Size {
		return nil, fail(CodeIndexConflict, "payload size")
	}
	return data, nil
}

// entryAt reads the journal entry at sequence through its immutable tag; a nonempty digest must match.
func (p *publisher) entryAt(ctx context.Context, sequence int64, digest string) (journalEntry, error) {
	e, err := p.entryByTag(ctx, descriptor.JournalTag(sequence))
	if errors.Is(err, errNotFound) {
		return journalEntry{}, fail(CodeIndexConflict, "journal entry missing")
	}
	if err != nil {
		return journalEntry{}, err
	}
	if e.entry.Sequence != sequence || (digest != "" && e.digest != digest) {
		return journalEntry{}, fail(CodeIndexConflict, "journal entry")
	}
	return e, nil
}

type journalEntry struct {
	entry    descriptor.Entry
	digest   string
	manifest []byte
}

func (p *publisher) entryByTag(ctx context.Context, tag string) (journalEntry, error) {
	m, err := p.reg.GetManifest(ctx, p.target.Repository, tag)
	if errors.Is(err, errNotFound) {
		return journalEntry{}, errNotFound
	}
	if err != nil {
		return journalEntry{}, registryFailure(err, "journal")
	}
	manifest, err := descriptor.ParseManifest(m.Body, descriptor.MediaTypeEntry)
	if err != nil || manifest.Created != "" || manifest.Subject != nil {
		return journalEntry{}, fail(CodeIndexConflict, "journal entry")
	}
	payload, err := p.blob(ctx, manifest.Layer)
	if err != nil {
		return journalEntry{}, err
	}
	entry, err := descriptor.ParseEntry(payload, p.target.Project)
	if err != nil {
		return journalEntry{}, failWith(CodeIndexConflict, "journal entry", err)
	}
	return journalEntry{entry: entry, digest: m.Digest, manifest: m.Body}, nil
}

// committed handles a retry whose receipt already exists: it verifies the receipt's entry describes exactly this
// prepared descriptor, that the descriptor tag still maps to it and that the entry is reachable from the current
// head, and returns that position without appending (§23.2).
func (p *publisher) committed(ctx context.Context, head headRead) (Result, bool, error) {
	receipt, err := p.entryByTag(ctx, p.receiptTag())
	if errors.Is(err, errNotFound) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if receipt.entry.DescriptorDigest != p.descriptor.Digest() || receipt.entry.PublicationTag != p.descriptorTag() {
		return Result{}, false, fail(CodePublicationConflict, "receipt")
	}
	if _, err := p.entryAt(ctx, receipt.entry.Sequence, receipt.digest); err != nil {
		return Result{}, false, err
	}
	if err := p.checkReleaseTag(ctx); err != nil {
		return Result{}, false, err
	}
	if receipt.entry.Sequence > head.head.Sequence {
		// A receipt beyond the head means the head write was lost after the receipt: finish it now.
		if receipt.entry.Sequence != head.head.Sequence+1 || !linksTo(receipt.entry, head.head) {
			return Result{}, false, fail(CodeIndexConflict, "receipt")
		}
		res, err := p.commitHead(ctx, head, receipt.entry.Sequence, receipt.digest)
		return res, true, err
	}
	if err := p.reachable(ctx, head.head, receipt.entry.Sequence, receipt.digest); err != nil {
		return Result{}, false, err
	}
	p.logf("release-descriptor: publication already committed at sequence %s", strconv.FormatInt(receipt.entry.Sequence, 10))
	published := p.state.PublishedAt
	if published == "" {
		published = p.now().Format(time.RFC3339) // the record was lost: the time this retry proved the commitment
	}
	return p.result(receipt.entry.Sequence, receipt.digest, published, true), true, nil
}

func linksTo(e descriptor.Entry, h descriptor.Head) bool {
	return e.Sequence == h.Sequence+1 && e.PreviousDigest == h.EntryDigest
}

// reachable walks previousDigest links from the head back to sequence, proving digest is on the committed chain.
func (p *publisher) reachable(ctx context.Context, head descriptor.Head, sequence int64, digest string) error {
	if head.Sequence-sequence > maxVerifyLinks {
		return fail(CodeIndexConflict, "journal too far to verify")
	}
	want := head.EntryDigest
	for s := head.Sequence; s > sequence; s-- {
		e, err := p.entryAt(ctx, s, want)
		if err != nil {
			return err
		}
		want = e.entry.PreviousDigest
	}
	if want != digest {
		return fail(CodeIndexConflict, "journal chain")
	}
	return nil
}

// upload publishes the payload, config and canonical manifest by digest in the descriptor repository, then every
// per-image referrer copy, then the canonical manifest under its immutable descriptor tag (§23.2 step 3).
func (p *publisher) upload(ctx context.Context, release descriptor.Release) error {
	if err := p.pushArtifact(ctx, p.target.Repository, p.state.Payload, p.descriptor); err != nil {
		return err
	}
	for _, img := range release.Images {
		_, repo, _ := descriptor.SplitImage(img.Image)
		copyManifest := p.copies[img.Image]
		if err := p.pushArtifact(ctx, repo, p.state.Payload, copyManifest); err != nil {
			return err
		}
		if err := p.indexReferrer(ctx, repo, copyManifest); err != nil {
			return err
		}
	}
	if _, err := p.putImmutable(ctx, p.descriptorTag(), descriptor.MediaTypeManifest, p.descriptor.Encode()); err != nil {
		return err
	}
	p.logf("release-descriptor: staged descriptor %s", p.descriptor.Digest())
	return nil
}

// pushArtifact uploads an artifact's config and payload blobs and its manifest by digest, then reads it back.
func (p *publisher) pushArtifact(ctx context.Context, repo string, payload []byte, m descriptor.Manifest) error {
	for _, blob := range [][]byte{descriptor.EmptyConfig, payload} {
		if err := p.reg.PutBlob(ctx, repo, blob); err != nil {
			return registryFailure(err, "upload")
		}
	}
	body := m.Encode()
	if err := p.reg.PutManifest(ctx, repo, m.Digest(), descriptor.MediaTypeManifest, body); err != nil {
		return registryFailure(err, "upload")
	}
	back, err := p.reg.GetManifest(ctx, repo, m.Digest())
	if err != nil {
		return registryFailure(err, "readback")
	}
	if !bytesEqual(back.Body, body) {
		return fail(CodeRegistryInvalid, "readback")
	}
	return nil
}

// checkDescriptorTag requires the immutable descriptor tag to map to exactly the prepared manifest.
func (p *publisher) checkReleaseTag(ctx context.Context) error {
	m, err := p.reg.GetManifest(ctx, p.target.Repository, p.descriptorTag())
	if errors.Is(err, errNotFound) {
		return fail(CodeIndexConflict, "descriptor tag missing")
	}
	if err != nil {
		return registryFailure(err, "descriptor tag")
	}
	if m.Digest != p.descriptor.Digest() {
		return fail(CodePublicationConflict, "descriptor tag")
	}
	return nil
}

// putImmutable writes an immutable tag in the descriptor repository. An existing tag with the exact manifest is
// reused; a different mapping is a conflict and is never overwritten. After a failed or ambiguous PUT the tag is read
// back before any retry. It reports whether the tag already existed.
func (p *publisher) putImmutable(ctx context.Context, tag, mediaType string, body []byte) (bool, error) {
	want := descriptor.SHA256(body)
	for attempt := 0; ; attempt++ {
		existing, err := p.reg.GetManifest(ctx, p.target.Repository, tag)
		switch {
		case err == nil && existing.Digest == want:
			return attempt == 0, nil
		case err == nil:
			return false, fail(CodePublicationConflict, "immutable tag")
		case !errors.Is(err, errNotFound):
			return false, registryFailure(err, "tag")
		case attempt >= retryAttempts:
			return false, fail(CodeRegistryUnavailable, "tag")
		}
		if err := p.reg.putOnce(ctx, p.target.Repository, tag, mediaType, body); err != nil && !retryable(err) {
			return false, registryFailure(err, "tag")
		}
	}
}

// append commits the publication: it first finishes any interrupted append found in the next slot, then creates this
// run's entry, writes its journal tag before its receipt tag, and writes the head last (§23.2 step 4).
func (p *publisher) append(ctx context.Context, head headRead) (Result, error) {
	for {
		if head.head.Sequence == math.MaxInt64 {
			return Result{}, fail(CodeJournalOverflow, "")
		}
		next := head.head.Sequence + 1
		slot, err := p.entryByTag(ctx, descriptor.JournalTag(next))
		if err != nil && !errors.Is(err, errNotFound) {
			return Result{}, err
		}
		if err == nil {
			// An interrupted append already wrote the next slot.
			if !linksTo(slot.entry, head.head) {
				return Result{}, fail(CodeIndexConflict, "journal slot")
			}
			if slot.entry.PublicationTag == p.descriptorTag() {
				if slot.entry.DescriptorDigest != p.descriptor.Digest() {
					return Result{}, fail(CodePublicationConflict, "journal slot")
				}
				return p.finish(ctx, head, slot)
			}
			if err := p.recoverSlot(ctx, head, slot); err != nil {
				return Result{}, err
			}
			if head, err = p.fetchHead(ctx); err != nil {
				return Result{}, err
			}
			continue
		}
		entry := descriptor.Entry{
			Project:          p.target.Project,
			Sequence:         next,
			PreviousDigest:   head.head.EntryDigest,
			DescriptorDigest: p.descriptor.Digest(),
			PublicationTag:   p.descriptorTag(),
		}
		payload, err := entry.Encode()
		if err != nil {
			return Result{}, failWith(CodeIndexConflict, "entry", err)
		}
		m := descriptor.JournalManifest(descriptor.MediaTypeEntry, payload)
		if err := p.pushArtifact(ctx, p.target.Repository, payload, m); err != nil {
			return Result{}, err
		}
		p.state.Sequence, p.state.EntryDigest = next, m.Digest()
		if err := p.state.save(p.opts.StateDir); err != nil {
			return Result{}, err
		}
		if _, err := p.putImmutable(ctx, descriptor.JournalTag(next), descriptor.MediaTypeManifest, m.Encode()); err != nil {
			return Result{}, err
		}
		return p.finish(ctx, head, journalEntry{entry: entry, digest: m.Digest(), manifest: m.Encode()})
	}
}

// finish writes this run's receipt tag and then the head.
func (p *publisher) finish(ctx context.Context, head headRead, e journalEntry) (Result, error) {
	if _, err := p.putImmutable(ctx, p.receiptTag(), descriptor.MediaTypeManifest, e.manifest); err != nil {
		return Result{}, err
	}
	return p.commitHead(ctx, head, e.entry.Sequence, e.digest)
}

// recoverSlot finishes another run's interrupted append: its descriptor, every image referrer and its immutable tags
// must verify before its receipt and head are written.
func (p *publisher) recoverSlot(ctx context.Context, head headRead, slot journalEntry) error {
	runID, runAttempt, _ := descriptor.ParseDescriptorTag(slot.entry.PublicationTag)
	m, err := p.reg.GetManifest(ctx, p.target.Repository, slot.entry.PublicationTag)
	if errors.Is(err, errNotFound) {
		return fail(CodeIndexConflict, "interrupted descriptor missing")
	}
	if err != nil {
		return registryFailure(err, "interrupted descriptor")
	}
	if m.Digest != slot.entry.DescriptorDigest {
		return fail(CodeIndexConflict, "interrupted descriptor")
	}
	manifest, err := descriptor.ParseManifest(m.Body, descriptor.MediaTypeRelease)
	if err != nil || manifest.Subject != nil || manifest.Created == "" {
		return fail(CodeIndexConflict, "interrupted descriptor")
	}
	payload, err := p.blob(ctx, manifest.Layer)
	if err != nil {
		return err
	}
	release, err := descriptor.ParseRelease(payload)
	if err != nil || release.Project != p.target.Project || release.RunID != runID || release.RunAttempt != runAttempt {
		return fail(CodeIndexConflict, "interrupted descriptor")
	}
	for i, img := range release.Images {
		host, repo, _ := descriptor.SplitImage(img.Image)
		if host != p.target.Host {
			return fail(CodeIndexConflict, "interrupted descriptor")
		}
		subject, err := p.subject(ctx, repo, img.Digest)
		if err != nil {
			return fmt.Errorf("images[%d]: %w", i, err)
		}
		copyManifest := descriptor.ReleaseManifest(payload, manifest.Created, &subject)
		back, err := p.reg.GetManifest(ctx, repo, copyManifest.Digest())
		if err != nil || !bytesEqual(back.Body, copyManifest.Encode()) {
			return fail(CodeIndexConflict, "interrupted referrer")
		}
	}
	if _, err := p.putImmutable(ctx, descriptor.ReceiptTag(runID, runAttempt), descriptor.MediaTypeManifest, slot.manifest); err != nil {
		return err
	}
	if _, err := p.commitHead(ctx, head, slot.entry.Sequence, slot.digest); err != nil {
		return err
	}
	p.logf("release-descriptor: recovered interrupted publication at sequence %s", strconv.FormatInt(slot.entry.Sequence, 10))
	return nil
}

// subject reads an image manifest by digest and returns its descriptor.
func (p *publisher) subject(ctx context.Context, repo, digest string) (descriptor.Descriptor, error) {
	m, err := p.reg.GetManifest(ctx, repo, digest)
	if err != nil {
		return descriptor.Descriptor{}, fail(CodeIndexConflict, "interrupted image")
	}
	v, ok := parseGraph(m.Body)
	if !ok {
		return descriptor.Descriptor{}, fail(CodeIndexConflict, "interrupted image")
	}
	mediaType, ok := mediaTypeOf(v, m.MediaType)
	if !ok {
		return descriptor.Descriptor{}, fail(CodeIndexConflict, "interrupted image")
	}
	return descriptor.Descriptor{MediaType: mediaType, Digest: m.Digest, Size: int64(len(m.Body))}, nil
}

// commitHead writes the head for (sequence, entry) last and verifies it. A failed or ambiguous head PUT is reconciled
// against the current head before any retry: our head means success, the expected previous head permits a retry, and
// anything else is a competing writer (§23.2 step 5).
func (p *publisher) commitHead(ctx context.Context, prev headRead, sequence int64, entry string) (Result, error) {
	h := descriptor.Head{Project: p.target.Project, Sequence: sequence, EntryDigest: entry}
	if err := p.writeHead(ctx, h, &prev); err != nil {
		return Result{}, err
	}
	published := p.now().Format(time.RFC3339)
	ours := p.state.Sequence == sequence && p.state.EntryDigest == entry
	if !ours {
		e, err := p.entryAt(ctx, sequence, entry)
		if err == nil && e.entry.PublicationTag == p.descriptorTag() {
			ours = true
		}
	}
	if !ours {
		return Result{}, nil // a recovered publication of another run
	}
	p.state.Sequence, p.state.EntryDigest, p.state.PublishedAt = sequence, entry, published
	if err := p.state.save(p.opts.StateDir); err != nil {
		return Result{}, err
	}
	p.logf("release-descriptor: published %s at journal sequence %s", p.descriptor.Digest(), strconv.FormatInt(sequence, 10))
	return p.result(sequence, entry, published, false), nil
}

// writeHead uploads the head artifact and writes the mutable head tag, reconciling an ambiguous write. prev is the
// head the write replaces (nil when initializing a missing head).
func (p *publisher) writeHead(ctx context.Context, h descriptor.Head, prev *headRead) error {
	payload, err := h.Encode()
	if err != nil {
		return failWith(CodeIndexConflict, "head", err)
	}
	m := descriptor.JournalManifest(descriptor.MediaTypeHead, payload)
	if err := p.pushArtifact(ctx, p.target.Repository, payload, m); err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		current, err := p.fetchHead(ctx)
		switch {
		case err == nil && current.digest == m.Digest():
			return nil
		case err == nil && (prev == nil || current.digest != prev.digest):
			return fail(CodeIndexConflict, "competing head writer")
		case errors.Is(err, errNotFound) && prev != nil:
			return fail(CodeIndexConflict, "head disappeared")
		case err != nil && !errors.Is(err, errNotFound):
			return err
		case attempt >= retryAttempts:
			return fail(CodeAmbiguousPublication, "head")
		}
		if err := p.reg.putOnce(ctx, p.target.Repository, descriptor.HeadTag, descriptor.MediaTypeManifest, m.Encode()); err != nil && !retryable(err) {
			return registryFailure(err, "head")
		}
	}
}

func (p *publisher) result(sequence int64, entry, published string, duplicate bool) Result {
	return Result{
		DescriptorRef:    p.target.Host + "/" + p.target.Repository + "@" + p.descriptor.Digest(),
		DescriptorDigest: p.descriptor.Digest(),
		PayloadDigest:    descriptor.SHA256(p.state.Payload),
		PublishedAt:      published,
		Sequence:         sequence,
		EntryDigest:      entry,
		Duplicate:        duplicate,
	}
}
