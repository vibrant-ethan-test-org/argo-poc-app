package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ExplainErrors attaches the contributing layer's source path to validation errors (§14.4).
// Only validated names and the validator's schema path are printed, never opaque deployer keys or values.
func ExplainErrors(p Pipeline, v Version, platformVersion int, errs []error) []error {
	return ExplainErrorsContext(context.Background(), p, v, platformVersion, errs)
}

// ExplainErrorsContext charges provenance indexing and checks caller cancellation.
func ExplainErrorsContext(ctx context.Context, p Pipeline, v Version, platformVersion int, errs []error) []error {
	if err := ExpansionStep(ctx, int64(len(v.Provenance))); err != nil {
		return []error{err}
	}
	// CFG-4: Expand bounds this input before any diagnostics index is built.
	if len(errs) == 0 || len(v.Provenance) > maxExpansionBytes {
		return errs
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(v.Provenance, &raw) != nil {
		return errs
	}
	// Index the lexicographically first descendant once, rather than scanning all
	// leaves per validation error (CFG-4).
	first := map[string]string{}
	for k := range raw {
		if k == "v" {
			continue
		}
		for prefix := k; prefix != ""; {
			if err := ExpansionStep(ctx, int64(len(prefix)+len(k))); err != nil {
				return []error{err}
			}
			if old, ok := first[prefix]; !ok || k < old {
				first[prefix] = k
			}
			j := strings.LastIndex(prefix, "/")
			prefix = prefix[:j]
		}
	}
	out := make([]error, 0, len(errs))
	for _, err := range errs {
		var pe *PathError
		if !errors.As(err, &pe) {
			out = append(out, err)
			continue
		}
		path := pe.Path
		ptr := "/" + strings.ReplaceAll(schemaIndices.ReplaceAllString(path, "/$1"), ".", "/")
		suffix := path
		if strings.HasPrefix(path, "stages[") {
			end := strings.IndexByte(path, ']')
			if end < 0 {
				out = append(out, err)
				continue
			}
			i, e := strconv.Atoi(path[7:end])
			if e != nil || i < 0 || i >= len(p.Stages) || !dnsLabel.MatchString(p.Stages[i].Name) {
				out = append(out, err)
				continue
			}
			suffix = path[end+1:]
			ptr = fmt.Sprintf("/stages/%d", i) + strings.ReplaceAll(schemaIndices.ReplaceAllString(suffix, "/$1"), ".", "/")
		}
		candidates := []string{}
		if k, ok := first[ptr]; ok {
			candidates = append(candidates, k)
		}
		if len(candidates) == 0 {
			for parent := ptr; parent != ""; {
				j := strings.LastIndex(parent, "/")
				parent = parent[:j]
				if _, ok := raw[parent]; ok {
					candidates = append(candidates, parent)
					break
				}
			}
		}
		if len(candidates) == 0 {
			out = append(out, err)
			continue
		}
		var origin Origin
		if json.Unmarshal(raw[candidates[0]], &origin) != nil {
			out = append(out, err)
			continue
		}
		location := origin.From
		if origin.From != FromBuiltin {
			sourcePath := origin.Path
			if origin.From == "default:platform" && strings.HasPrefix(path, "stages[") {
				sourcePath = "defaults" + suffix
			}
			// A plugin validates opaque config as a whole: locate the schema field, not an arbitrary leaf key.
			if strings.HasPrefix(path, "stages[") {
				if end := strings.Index(sourcePath, "].stages["); end >= 0 {
					end += len("].stages[")
					if endStage := strings.IndexByte(sourcePath[end:], ']'); endStage >= 0 {
						sourcePath = sourcePath[:end+endStage+1] + suffix
					}
				} else if end := strings.IndexByte(sourcePath, ']'); end >= 0 {
					sourcePath = sourcePath[:end+1] + suffix
				}
			}
			location += " " + sourcePath
		}
		if strings.HasPrefix(origin.From, "template:") || origin.From == "default:platform" {
			location += fmt.Sprintf(" at platform v%d", platformVersion)
		}
		out = append(out, &PathError{pe.Path, fmt.Errorf("%w (from %s)", pe.Err, location)})
	}
	return out
}

var schemaIndices = regexp.MustCompile(`\[([0-9]+)\]`)
