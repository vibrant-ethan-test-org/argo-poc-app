package publish

import (
	"context"
	"errors"
	"fmt"

	"github.com/Vibrant-America/promotion-controller/internal/descriptor"
)

// Image graph bounds (§23.6).
const (
	maxPlatforms    = 32
	maxIndexEntries = 64
	graphDepth      = 16
	graphNodes      = 100000
)

func parseGraph(data []byte) (descriptor.Value, bool) {
	v, err := descriptor.Parse(data, descriptor.Limits{Bytes: maxResponseBytes, Depth: graphDepth, Nodes: graphNodes})
	return v, err == nil && v.Kind == descriptor.Object
}

func get(v descriptor.Value, key string) descriptor.Value {
	for _, m := range v.Members {
		if m.Key == key {
			return m.Value
		}
	}
	return descriptor.Value{Kind: descriptor.Null}
}

// annotation returns an object's string annotation, and whether it is present.
func annotation(v descriptor.Value, key string) (string, bool, bool) {
	a := get(v, "annotations")
	if a.Kind == descriptor.Null {
		return "", false, true
	}
	if a.Kind != descriptor.Object {
		return "", false, false
	}
	val := get(a, key)
	switch val.Kind {
	case descriptor.Null:
		return "", false, true
	case descriptor.String:
		return val.Str, true, true
	}
	return "", false, false
}

func isIndex(mediaType string) bool {
	return mediaType == descriptor.MediaTypeIndex || mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
}

// mediaTypeOf takes a manifest's media type from its body, else from Content-Type; both, when present, must agree.
func mediaTypeOf(v descriptor.Value, header string) (string, bool) {
	body := get(v, "mediaType")
	switch {
	case body.Kind == descriptor.String && header != "" && descriptor.SubjectMediaType(header) && body.Str != header:
		return "", false
	case body.Kind == descriptor.String:
		return body.Str, descriptor.SubjectMediaType(body.Str)
	case body.Kind != descriptor.Null:
		return "", false
	}
	return header, descriptor.SubjectMediaType(header)
}

// inspectImage verifies one release image before anything is published (§23.2 step 1, mirroring §23.3 step 1): the
// digest exists and matches its bytes, it is an image manifest or index of at most 32 platforms and depth 2, and
// every platform carries the release commit as its revision, in its manifest annotation or config label, with every
// present revision agreeing. Config blobs are read even when annotations are present. It returns the image's subject
// descriptor for its referrer copy.
func (p *publisher) inspectImage(ctx context.Context, i int, img descriptor.Image, commit string) (descriptor.Descriptor, error) {
	_, repo, _ := descriptor.SplitImage(img.Image)
	label := fmt.Sprintf("images[%d]", i) // a schema path: errors never echo the image name
	m, err := p.reg.GetManifest(ctx, repo, img.Digest)
	if errors.Is(err, errNotFound) {
		return descriptor.Descriptor{}, fail(CodeImageMissing, label)
	}
	if err != nil {
		return descriptor.Descriptor{}, registryFailure(err, "image")
	}
	v, ok := parseGraph(m.Body)
	if !ok {
		return descriptor.Descriptor{}, fail(CodeImageInvalid, label)
	}
	mediaType, ok := mediaTypeOf(v, m.MediaType)
	if !ok {
		return descriptor.Descriptor{}, fail(CodeImageInvalid, label)
	}
	if isIndex(mediaType) {
		err = p.inspectIndex(ctx, repo, label, v, commit)
	} else {
		err = p.inspectPlatform(ctx, repo, label, v, commit)
	}
	if err != nil {
		return descriptor.Descriptor{}, err
	}
	return descriptor.Descriptor{MediaType: mediaType, Digest: m.Digest, Size: int64(len(m.Body))}, nil
}

func (p *publisher) inspectIndex(ctx context.Context, repo, label string, idx descriptor.Value, commit string) error {
	if rev, present, ok := annotation(idx, descriptor.AnnotationRevision); !ok || (present && rev != commit) {
		return fail(CodeRevisionMismatch, label)
	}
	entries := get(idx, "manifests")
	if entries.Kind != descriptor.Array || len(entries.Items) > maxIndexEntries {
		return fail(CodeImageInvalid, label)
	}
	platforms := 0
	for _, entry := range entries.Items {
		if entry.Kind != descriptor.Object {
			return fail(CodeImageInvalid, label)
		}
		if attestation(entry) {
			continue
		}
		platforms++
		digest, mediaType := get(entry, "digest"), get(entry, "mediaType")
		if platforms > maxPlatforms || digest.Kind != descriptor.String || !descriptor.Digest(digest.Str) ||
			mediaType.Kind != descriptor.String || isIndex(mediaType.Str) || !descriptor.SubjectMediaType(mediaType.Str) {
			return fail(CodeImageInvalid, label) // nested indexes are unsupported graphs
		}
		m, err := p.reg.GetManifest(ctx, repo, digest.Str)
		if errors.Is(err, errNotFound) {
			return fail(CodeImageMissing, label)
		}
		if err != nil {
			return registryFailure(err, "image")
		}
		v, ok := parseGraph(m.Body)
		if !ok {
			return fail(CodeImageInvalid, label)
		}
		if mt, ok := mediaTypeOf(v, m.MediaType); !ok || isIndex(mt) {
			return fail(CodeImageInvalid, label)
		}
		if err := p.inspectPlatform(ctx, repo, label, v, commit); err != nil {
			return err
		}
	}
	if platforms == 0 {
		return fail(CodeImageInvalid, label)
	}
	return nil
}

// attestation recognises the provenance and SBOM entries buildx adds to an index (as ingest does).
func attestation(entry descriptor.Value) bool {
	if kind, present, _ := annotation(entry, "vnd.docker.reference.type"); present && kind == "attestation-manifest" {
		return true
	}
	platform := get(entry, "platform")
	return platform.Kind == descriptor.Object && get(platform, "os").Str == "unknown" &&
		get(platform, "architecture").Str == "unknown"
}

func (p *publisher) inspectPlatform(ctx context.Context, repo, label string, m descriptor.Value, commit string) error {
	rev, annotated, ok := annotation(m, descriptor.AnnotationRevision)
	if !ok || (annotated && rev != commit) {
		return fail(CodeRevisionMismatch, label)
	}
	cfg := get(m, "config")
	digest := get(cfg, "digest")
	if cfg.Kind != descriptor.Object || digest.Kind != descriptor.String || !descriptor.Digest(digest.Str) {
		return fail(CodeImageInvalid, label)
	}
	raw, err := p.reg.GetBlob(ctx, repo, digest.Str, maxResponseBytes)
	if errors.Is(err, errNotFound) {
		return fail(CodeImageMissing, label)
	}
	if err != nil {
		return registryFailure(err, "image config")
	}
	config, ok := parseGraph(raw)
	if !ok {
		return fail(CodeImageInvalid, label)
	}
	labels := get(get(config, "config"), "Labels")
	revision := get(labels, descriptor.AnnotationRevision)
	labelled := labels.Kind == descriptor.Object && revision.Kind == descriptor.String
	if labels.Kind != descriptor.Null && labels.Kind != descriptor.Object || revision.Kind != descriptor.Null && revision.Kind != descriptor.String {
		return fail(CodeImageInvalid, label)
	}
	if (labelled && revision.Str != commit) || (!labelled && !annotated) {
		return fail(CodeRevisionMismatch, label)
	}
	return nil
}

// registryFailure maps a registry error to its fixed code.
func registryFailure(err error, step string) error {
	var f *Failure
	if errors.As(err, &f) {
		return err
	}
	if errors.Is(err, errNotFound) {
		return failWith(CodeRegistryInvalid, step, err)
	}
	return failWith(CodeRegistryUnavailable, step, fmt.Errorf("%s: %w", step, err))
}
