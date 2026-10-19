package descriptor

import "bytes"

// Bounds for manifests read back from a registry (§23.6).
const (
	MaxManifestBytes    = 4 << 20
	maxManifestDepth    = 16
	maxManifestNodes    = 100000
	maxSubjectBytes     = 4 << 20
	manifestSchema      = 2
	mediaDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

// Descriptor is an OCI content descriptor: media type, digest and size, with no URLs, annotations or inline data.
type Descriptor struct {
	MediaType string
	Digest    string
	Size      int64
}

// Manifest is a publisher-generated OCI artifact manifest (§23.1, §23.2): empty config, exactly one layer, an optional
// creation annotation and an optional subject. Its artifact type equals its layer's media type.
type Manifest struct {
	Layer   Descriptor
	Created string      // org.opencontainers.image.created; empty for none
	Subject *Descriptor // per-image copies only
}

// SubjectMediaType reports the media types a release may reference as an image: OCI and Docker manifests and indexes.
func SubjectMediaType(s string) bool {
	switch s {
	case MediaTypeManifest, MediaTypeIndex, mediaDockerList, mediaDockerManifest:
		return true
	}
	return false
}

// ReleaseManifest is the manifest for a canonical release payload. With a nil subject it is the canonical,
// subject-free publication manifest; with a subject it is that image's referrer copy (§23.2).
func ReleaseManifest(payload []byte, created string, subject *Descriptor) Manifest {
	return Manifest{
		Layer:   Descriptor{MediaType: MediaTypeRelease, Digest: SHA256(payload), Size: int64(len(payload))},
		Created: created,
		Subject: subject,
	}
}

// JournalManifest is the manifest of a head or entry payload, of the given media type.
func JournalManifest(mediaType string, payload []byte) Manifest {
	return Manifest{Layer: Descriptor{MediaType: mediaType, Digest: SHA256(payload), Size: int64(len(payload))}}
}

func descriptorValue(d Descriptor) Value {
	return Obj(Member{"mediaType", Str(d.MediaType)}, Member{"digest", Str(d.Digest)}, Member{"size", Int(d.Size)})
}

// Encode returns the canonical (RFC 8785) manifest bytes.
func (m Manifest) Encode() []byte {
	members := []Member{
		{"schemaVersion", Int(manifestSchema)},
		{"mediaType", Str(MediaTypeManifest)},
		{"artifactType", Str(m.Layer.MediaType)},
		{"config", descriptorValue(Descriptor{MediaType: MediaTypeEmpty, Digest: EmptyConfigDigest, Size: int64(len(EmptyConfig))})},
		{"layers", Arr(descriptorValue(m.Layer))},
	}
	if m.Created != "" {
		members = append(members, Member{"annotations", Obj(Member{AnnotationCreated, Str(m.Created)})})
	}
	if m.Subject != nil {
		members = append(members, Member{"subject", descriptorValue(*m.Subject)})
	}
	return Canonical(Obj(members...))
}

// Digest returns the SHA-256 digest of the canonical manifest bytes.
func (m Manifest) Digest() string { return SHA256(m.Encode()) }

// ParseManifest strictly decodes a publisher-generated manifest of the given layer media type. It rejects any other
// shape (image layers, URLs, inline data, extra annotations or fields) and requires canonical bytes.
func ParseManifest(data []byte, layerType string) (Manifest, error) {
	bad := invalid(CodeManifestInvalid, "")
	v, err := Parse(data, Limits{Bytes: MaxManifestBytes, Depth: maxManifestDepth, Nodes: maxManifestNodes})
	if err != nil {
		return Manifest{}, bad
	}
	top, ok := fields(v, []string{"schemaVersion", "mediaType", "artifactType", "config", "layers"}, "annotations", "subject")
	if !ok || top["schemaVersion"].Kind != Number || top["schemaVersion"].Int != manifestSchema ||
		top["mediaType"].Str != MediaTypeManifest || top["artifactType"].Str != layerType {
		return Manifest{}, bad
	}
	cfg, ok := parseDescriptor(top["config"])
	if !ok || cfg != (Descriptor{MediaType: MediaTypeEmpty, Digest: EmptyConfigDigest, Size: int64(len(EmptyConfig))}) {
		return Manifest{}, bad
	}
	layers := top["layers"]
	if layers.Kind != Array || len(layers.Items) != 1 {
		return Manifest{}, bad
	}
	var m Manifest
	if m.Layer, ok = parseDescriptor(layers.Items[0]); !ok || m.Layer.MediaType != layerType {
		return Manifest{}, bad
	}
	if a, found := top["annotations"]; found {
		ann, ok := fields(a, []string{AnnotationCreated})
		if !ok || ann[AnnotationCreated].Kind != String || !validTimestamp(ann[AnnotationCreated].Str) {
			return Manifest{}, bad
		}
		m.Created = ann[AnnotationCreated].Str
	}
	if s, found := top["subject"]; found {
		subject, ok := parseDescriptor(s)
		if !ok || !SubjectMediaType(subject.MediaType) || subject.Size > maxSubjectBytes {
			return Manifest{}, bad
		}
		m.Subject = &subject
	}
	if !bytes.Equal(m.Encode(), data) {
		return Manifest{}, bad
	}
	return m, nil
}

func parseDescriptor(v Value) (Descriptor, bool) {
	f, ok := fields(v, []string{"mediaType", "digest", "size"})
	if !ok || f["mediaType"].Kind != String || f["digest"].Kind != String || f["size"].Kind != Number ||
		!Digest(f["digest"].Str) || f["size"].Int < 1 || len(f["mediaType"].Str) > 255 {
		return Descriptor{}, false
	}
	return Descriptor{MediaType: f["mediaType"].Str, Digest: f["digest"].Str, Size: f["size"].Int}, true
}
