package pipeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"

	yamlstream "go.yaml.in/yaml/v2"
	"sigs.k8s.io/yaml"
)

// APIVersion is the supported pipeline schema (docs/design/08-config.md §8.1).
const APIVersion = "promotion.vibrantsci.com/v1alpha1"

// DefaultTagPattern matches semantic version image tags (§8.1).
const DefaultTagPattern = `^v\d+\.\d+\.\d+$`

// Pipeline is the per-project registration document (D-025).
type Pipeline struct {
	APIVersion string  `json:"apiVersion"`
	Project    string  `json:"project"`
	Extends    string  `json:"extends,omitempty"`
	Owner      Owner   `json:"owner"`
	Source     Source  `json:"source"`
	Stages     []Stage `json:"stages"`
}

// Owner identifies the team and notification destination responsible for a project.
type Owner struct {
	Team  string `json:"team"`
	Slack string `json:"slack"`
}

// Source requires a build for every listed image (D-041).
// Descriptor and each image's Registry are the optional descriptor-mode fields of §8.8 (D-072). They are validated
// strictly now, so the F103 publisher and F104 ingest share one validator; ingest ignores them until F104.
type Source struct {
	Repo       string      `json:"repo"`
	Branch     string      `json:"branch"`
	Images     []Image     `json:"images"`
	Descriptor *Descriptor `json:"descriptor,omitempty"`
}

// Image selects a source image and the tags eligible for ingestion.
type Image struct {
	Image      string `json:"image"`
	Registry   string `json:"registry,omitempty"`
	TagPattern string `json:"tagPattern"`
}

// Descriptor locates a project's release descriptors: a registry binding name and a repository on that registry
// (§8.8, docs/design/23-release-descriptors.md §23.2).
type Descriptor struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
}

// Stage has a single upstream (D-008); plugin configuration stays opaque.
type Stage struct {
	Name          string                       `json:"name"`
	Upstream      string                       `json:"upstream,omitempty"`
	Deploy        map[string]json.RawMessage   `json:"deploy"`
	Gate          []map[string]json.RawMessage `json:"gate,omitempty"`
	OnGateFailure string                       `json:"onGateFailure,omitempty"`
	Windows       []string                     `json:"windows,omitempty"`
}

// PathError locates a decoding or validation failure in the YAML document. Neither part echoes the document: Path is
// made of schema field names, indices and registered plugin names, and Err is a fixed description that quotes no
// value, no unknown key and no library message, because the API returns it and stores it in the audit (principle 18,
// docs/design/14-config-versions.md §14.2).
type PathError struct {
	Path string
	Err  error
}

func (e *PathError) Error() string { return e.Path + ": " + e.Err.Error() }
func (e *PathError) Unwrap() error { return e.Err }

// Decode strictly decodes at most 1 MiB of §8.1 without applying defaults.
// Plugin-owned fields are checked by their registered Validator during Validate (§8.2).
func Decode(data []byte) (Pipeline, error) {
	p, _, err := decode(data)
	return p, err
}

// decode is Decode that also returns the document as JSON, before defaults: the source of a version (§14.1).
func decode(data []byte, expansion ...bool) (Pipeline, json.RawMessage, error) {
	var p Pipeline
	// Bound parsing work before either YAML decoder processes untrusted input.
	if len(data) > 1<<20 {
		return p, nil, &PathError{"$", fmt.Errorf("pipeline input exceeds 1 MiB limit (1048576 bytes)")}
	}
	// YAMLToJSONStrict reads one document; refuse trailing documents rather than
	// silently ignoring part of a registration file.
	if err := checkDecodeLimits(data, "project", expansion...); err != nil {
		return p, nil, err
	}
	stream := yamlstream.NewDecoder(bytes.NewReader(data))
	stream.SetStrict(true)
	var document any
	if err := stream.Decode(&document); err != nil {
		return p, nil, &PathError{"$", yamlError(err)}
	}
	if err := stream.Decode(&document); err != io.EOF {
		if err == nil {
			return p, nil, &PathError{"$", fmt.Errorf("decode YAML: expected exactly one YAML document")}
		}
		return p, nil, &PathError{"$", yamlError(err)}
	}
	raw, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return p, nil, &PathError{"$", yamlError(err)}
	}
	if err := checkShape(raw, reflect.TypeOf(p), ""); err != nil {
		return p, nil, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, nil, &PathError{"$", fmt.Errorf("decode pipeline: a value has the wrong type")}
	}
	return p, raw, nil
}

// yamlLine finds the line number in a YAML library's error.
var yamlLine = regexp.MustCompile(`\bline ([0-9]+)\b`)

// yamlLimits are the YAML library's resource limits, whose messages quote nothing of the document (CFG-4).
var yamlLimits = []string{"excessive aliasing", "exceeded max depth"}

// yamlError describes a YAML library's error by its line alone, or by the resource limit it hit: the libraries quote
// the document's keys and values in their other messages (a duplicate key, a value that doesn't fit), which errors
// must not echo.
func yamlError(err error) error {
	msg := err.Error()
	for _, limit := range yamlLimits {
		if strings.Contains(msg, limit) {
			return fmt.Errorf("decode YAML: %s", limit)
		}
	}
	if m := yamlLine.FindStringSubmatch(msg); m != nil {
		return fmt.Errorf("decode YAML: invalid YAML at line %s", m[1])
	}
	return fmt.Errorf("decode YAML: invalid YAML")
}

func child(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// Walk the schema before unmarshalling so errors include sequence indices.
func checkShape(raw json.RawMessage, typ reflect.Type, path string) error {
	fail := func(err error) error {
		if path == "" {
			return &PathError{"$", err}
		}
		return &PathError{path, err}
	}
	if typ == reflect.TypeFor[json.RawMessage]() {
		return nil
	}
	if string(raw) == "null" {
		if strings.HasSuffix(path, ".upstream") || strings.HasSuffix(path, ".gate") || strings.HasSuffix(path, ".windows") {
			return nil
		}
		if strings.HasSuffix(path, ".onGateFailure") {
			return fail(fmt.Errorf("null is not allowed (keep or rollback)"))
		}
		return fail(fmt.Errorf("must not be null"))
	}
	switch typ.Kind() {
	case reflect.Pointer:
		return checkShape(raw, typ.Elem(), path)
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return fail(fmt.Errorf("expected mapping"))
		}
		known := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			known[name] = f.Type
		}
		for _, key := range sortedKeys(fields) {
			fieldType, ok := known[key]
			if !ok {
				// The mapping's path, not the key's: an unknown key is input.
				return fail(fmt.Errorf("unknown field (allowed: %s)", strings.Join(sortedKeys(known), ", ")))
			}
			if err := checkShape(fields[key], fieldType, child(path, key)); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return fail(fmt.Errorf("expected sequence"))
		}
		for i, v := range values {
			if err := checkShape(v, typ.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		var values map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return fail(fmt.Errorf("expected mapping"))
		}
	case reflect.String:
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return fail(fmt.Errorf("expected string"))
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
