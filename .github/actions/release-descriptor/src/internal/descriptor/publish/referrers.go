package publish

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Vibrant-America/promotion-controller/internal/descriptor"
)

const maxReferrers = 10000

// indexReferrer makes a per-image copy discoverable from its subject (§23.2 step 3). With the referrers API the
// registry indexes it and the copy must appear in the subject's referrers. Without it, the OCI referrers tag schema
// index (tag "sha256-<hex>") is maintained by read, merge and write, and read back.
func (p *publisher) indexReferrer(ctx context.Context, repo string, m descriptor.Manifest) error {
	digest := m.Digest()
	for attempt := 0; attempt < retryAttempts; attempt++ {
		digests, supported, err := p.reg.Referrers(ctx, repo, m.Subject.Digest, descriptor.MediaTypeRelease)
		if err != nil {
			return registryFailure(err, "referrers")
		}
		if !supported {
			return p.mergeReferrersTag(ctx, repo, m)
		}
		for _, d := range digests {
			if d == digest {
				return nil
			}
		}
		if err := p.reg.sleep(ctx, backoff(attempt)); err != nil {
			return registryFailure(err, "referrers")
		}
	}
	return fail(CodeRegistryInvalid, "referrers")
}

func (p *publisher) mergeReferrersTag(ctx context.Context, repo string, m descriptor.Manifest) error {
	tag := "sha256-" + strings.TrimPrefix(m.Subject.Digest, "sha256:")
	digest := m.Digest()
	entry := descriptor.Obj(
		descriptor.Member{Key: "mediaType", Value: descriptor.Str(descriptor.MediaTypeManifest)},
		descriptor.Member{Key: "digest", Value: descriptor.Str(digest)},
		descriptor.Member{Key: "size", Value: descriptor.Int(int64(len(m.Encode())))},
		descriptor.Member{Key: "artifactType", Value: descriptor.Str(descriptor.MediaTypeRelease)},
		descriptor.Member{Key: "annotations", Value: descriptor.Obj(descriptor.Member{Key: descriptor.AnnotationCreated, Value: descriptor.Str(m.Created)})},
	)
	for attempt := 0; attempt < retryAttempts; attempt++ {
		doc := descriptor.Obj(
			descriptor.Member{Key: "schemaVersion", Value: descriptor.Int(2)},
			descriptor.Member{Key: "mediaType", Value: descriptor.Str(descriptor.MediaTypeIndex)},
			descriptor.Member{Key: "manifests", Value: descriptor.Arr()},
		)
		existing, err := p.reg.GetManifest(ctx, repo, tag)
		switch {
		case errors.Is(err, errNotFound):
		case err != nil:
			return registryFailure(err, "referrers index")
		default:
			v, found, ok := referrerEntries(existing.Body, digest)
			if !ok {
				return fail(CodeRegistryInvalid, "referrers index")
			}
			if found {
				return nil
			}
			doc = v
		}
		for i := range doc.Members {
			if doc.Members[i].Key == "manifests" {
				if len(doc.Members[i].Value.Items) >= maxReferrers {
					return fail(CodeRegistryInvalid, "referrers index")
				}
				doc.Members[i].Value.Items = append(doc.Members[i].Value.Items, entry)
			}
		}
		if err := p.reg.PutManifest(ctx, repo, tag, descriptor.MediaTypeIndex, descriptor.Canonical(doc)); err != nil {
			return registryFailure(err, "referrers index")
		}
		back, err := p.reg.GetManifest(ctx, repo, tag)
		if err == nil {
			if _, found, ok := referrerEntries(back.Body, digest); ok && found {
				return nil
			}
		}
		// A concurrent writer replaced the index between our read and write: merge again.
		if err := p.reg.sleep(ctx, time.Duration(attempt+1)*100*time.Millisecond); err != nil {
			return registryFailure(err, "referrers index")
		}
	}
	return fail(CodeRegistryUnavailable, "referrers index")
}

// referrerEntries parses an OCI image index and reports whether it lists digest. ok is false for anything that is not
// an index with a manifests array.
func referrerEntries(data []byte, digest string) (descriptor.Value, bool, bool) {
	v, ok := parseGraph(data)
	if !ok || get(v, "schemaVersion").Kind != descriptor.Number || get(v, "schemaVersion").Int != 2 {
		return descriptor.Value{}, false, false
	}
	if mt := get(v, "mediaType"); mt.Kind != descriptor.Null && mt.Str != descriptor.MediaTypeIndex {
		return descriptor.Value{}, false, false
	}
	manifests := get(v, "manifests")
	if manifests.Kind != descriptor.Array {
		return descriptor.Value{}, false, false
	}
	for _, m := range manifests.Items {
		if get(m, "digest").Kind == descriptor.String && get(m, "digest").Str == digest {
			return v, true, true
		}
	}
	return v, false, true
}
