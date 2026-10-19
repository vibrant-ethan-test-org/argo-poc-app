// Package descriptor holds the release-descriptor wire contract of docs/design/23-release-descriptors.md (D-072): the
// strict JSON parser and RFC 8785 encoder, the release payload, its OCI manifests, the publication journal and the
// shared syntax validators. The F103 publisher and F104 ingest both use it. It imports only the standard library and
// does no I/O.
package descriptor
