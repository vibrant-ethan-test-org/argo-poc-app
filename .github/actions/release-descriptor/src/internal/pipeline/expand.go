package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Template is a named, partial stage list; templates cannot chain (D-035).
type Template struct {
	Name   string            `json:"name"`
	Stages []json.RawMessage `json:"stages"`
}

// Defaults supplies the platform layer, below template and project values (§14.3).
type Defaults struct {
	OnGateFailure *string                    `json:"onGateFailure,omitempty"`
	TagPattern    *string                    `json:"tagPattern,omitempty"`
	Deploy        map[string]json.RawMessage `json:"deploy,omitempty"`
}

func checkTemplates(p Platform) error {
	names := map[string]bool{}
	for i, t := range p.Templates {
		path := fmt.Sprintf("templates[%d]", i)
		if len(t.Name) > 63 || !dnsLabel.MatchString(t.Name) {
			return &PathError{path + ".name", fmt.Errorf("must be a DNS label of 1–63 characters")}
		}
		if names[t.Name] {
			return &PathError{path + ".name", fmt.Errorf("duplicate template name")}
		}
		names[t.Name] = true
		stages := map[string]bool{}
		for j, raw := range t.Stages {
			sp := fmt.Sprintf("%s.stages[%d]", path, j)
			if err := checkShape(raw, reflect.TypeFor[Stage](), sp); err != nil {
				return err
			}
			var st Stage
			if err := json.Unmarshal(raw, &st); err != nil {
				return &PathError{sp, fmt.Errorf("invalid stage")}
			}
			if len(st.Name) > 63 || !dnsLabel.MatchString(st.Name) {
				return &PathError{sp + ".name", fmt.Errorf("must be a DNS label of 1–63 characters")}
			}
			if stages[st.Name] {
				return &PathError{sp + ".name", fmt.Errorf("duplicate stage name")}
			}
			stages[st.Name] = true
		}
	}
	return nil
}

// Expand applies the four merge-patch layers and substitutes string values (D-035).
// Platform version is stored separately from identity: unaffected expansions retain their digest (§14.1).
func Expand(ctx context.Context, data []byte, platform Platform, platformVersion int) (result Pipeline, version Version, failure error) {
	work := &expansionWork{ctx: ctx}
	// Helpers stop immediately on exhaustion; preserve the resource error over semantic diagnostics.
	defer func() {
		work.step(0) // include cancellation during bounded serialization or hashing
		if work.err != nil {
			result, version, failure = Pipeline{}, Version{}, work.err
		}
	}()
	if !work.step(int64(len(data)) + 1) {
		return Pipeline{}, Version{}, work.err
	}
	p, raw, err := decode(data, true)
	if err != nil {
		return p, Version{}, err
	}
	var doc map[string]any
	if err := decodeNumbers(raw, &doc); err != nil {
		return p, Version{}, &PathError{"$", fmt.Errorf("invalid pipeline object")}
	}
	var templ Template
	if p.Extends != "" {
		found := false
		for _, t := range platform.Templates {
			if !work.step(1) {
				return p, Version{}, work.err
			}
			if t.Name == p.Extends {
				templ = t
				found = true
				break
			}
		}
		if !found {
			return p, Version{}, &PathError{"extends", fmt.Errorf("unknown template")}
		}
	}
	origins := map[string]Origin{}
	// Merge patches also track which layer last supplied each leaf.
	merge := func(dst map[string]any, patch map[string]any, pointer, path, from string) {
		mergeLayer(work, dst, patch, pointer, path, from, origins)
	}
	stages := []any{}
	projectStages, _ := doc["stages"].([]any)
	templateStages := []any{}
	for _, r := range templ.Stages {
		if !work.step(int64(len(r))) {
			return p, Version{}, work.err
		}
		if !work.step(1) {
			return p, Version{}, work.err
		}
		var st any
		if err := checkDecodeLimits(r, "platform", true); err != nil {
			return p, Version{}, err
		}
		if err := decodeNumbers(r, &st); err != nil {
			return p, Version{}, &PathError{"templates", fmt.Errorf("invalid stage object")}
		}
		templateStages = append(templateStages, st)
	}
	order := []string{}
	tm := map[string]map[string]any{}
	pm := map[string]map[string]any{}
	for _, v := range templateStages {
		if !work.step(1) {
			return p, Version{}, work.err
		}
		st := v.(map[string]any)
		n, _ := st["name"].(string)
		order = append(order, n)
		tm[n] = st
	}
	for _, v := range projectStages {
		if !work.step(1) {
			return p, Version{}, work.err
		}
		st := v.(map[string]any)
		n, _ := st["name"].(string)
		if _, ok := pm[n]; ok {
			return p, Version{}, &RuleError{&PathError{"stages", fmt.Errorf("duplicate stage name")}}
		}
		pm[n] = st
		if _, ok := tm[n]; !ok {
			order = append(order, n)
		}
	}
	for i, n := range order {
		if !work.step(1) {
			return p, Version{}, work.err
		}
		pointer := fmt.Sprintf("/stages/%d", i)
		path := "stages[" + n + "]"
		dst := map[string]any{}
		merge(dst, map[string]any{"onGateFailure": "keep"}, pointer, "", FromBuiltin)
		defaults := map[string]any{}
		if platform.Defaults.OnGateFailure != nil {
			defaults["onGateFailure"] = *platform.Defaults.OnGateFailure
		}
		// The highest layer that names a deployer chooses the only plugin whose defaults apply.
		plugin := ""
		for _, st := range []map[string]any{tm[n], pm[n]} {
			if !work.step(1) {
				return p, Version{}, work.err
			}
			if d, ok := st["deploy"].(map[string]any); ok {
				plugin = ""
				if len(d) == 1 {
					for k := range d {
						if !work.step(1) {
							return p, Version{}, work.err
						}
						plugin = k
					}
				}
			}
		}
		if d, ok := platform.Defaults.Deploy[plugin]; ok {
			if !work.step(int64(len(d))) {
				return p, Version{}, work.err
			}
			var cfg any
			if err := checkDecodeLimits(d, "platform", true); err != nil {
				return p, Version{}, err
			}
			if err := decodeNumbers(d, &cfg); err != nil {
				return p, Version{}, &PathError{"defaults.deploy", fmt.Errorf("invalid deploy object")}
			}
			defaults["deploy"] = map[string]any{plugin: cfg}
		}
		merge(dst, defaults, pointer, "defaults", "default:platform")
		for j, st := range []map[string]any{tm[n], pm[n]} {
			if !work.step(1) {
				return p, Version{}, work.err
			}
			if st == nil {
				continue
			}
			patch := map[string]any{}
			for k, v := range st {
				if !work.step(1) {
					return p, Version{}, work.err
				}
				patch[k] = v
			}
			if d, ok := patch["deploy"].(map[string]any); ok {
				if _, same := d[plugin]; !same {
					delete(patch, "deploy")
				}
			}
			from, src := FromProject, path
			if j == 0 {
				from = "template:" + templ.Name
				src = "templates[" + templ.Name + "]." + path
			}
			merge(dst, patch, pointer, src, from)
		}
		stages = append(stages, dst)
	}
	doc["stages"] = stages
	// Project-only owner/source, with independently layered image tag patterns.
	for _, key := range []string{"owner", "source"} {
		if !work.step(1) {
			return p, Version{}, work.err
		}
		recordOrigins(work, doc[key], "/"+key, key, FromProject, origins)
	}
	src, _ := doc["source"].(map[string]any)
	if images, ok := src["images"].([]any); ok {
		for i, v := range images {
			if !work.step(1) {
				return p, Version{}, work.err
			}
			img := v.(map[string]any)
			ptr := fmt.Sprintf("/source/images/%d/tagPattern", i)
			if _, set := img["tagPattern"]; !set {
				img["tagPattern"] = DefaultTagPattern
				origins[ptr] = Origin{From: FromBuiltin}
				if platform.Defaults.TagPattern != nil {
					img["tagPattern"] = *platform.Defaults.TagPattern
					origins[ptr] = Origin{From: "default:platform", Path: "defaults.tagPattern"}
				}
			}
		}
	}
	for i, st := range stages {
		if !work.step(1) {
			return p, Version{}, work.err
		}
		if err := substitute(work, st, p.Project, order[i], fmt.Sprintf("stages[%d]", i)); err != nil {
			return p, Version{}, err
		}
	}
	if err := substitute(work, src, p.Project, "", "source"); err != nil {
		return p, Version{}, err
	}
	if err := substitute(work, doc["owner"], p.Project, "", "owner"); err != nil {
		return p, Version{}, err
	}
	work.output = 0
	if err := chargeValue(work, doc, 0); err != nil {
		return p, Version{}, err
	}
	expandedRaw, err := json.Marshal(doc)
	if err != nil {
		return p, Version{}, err
	}
	p = Pipeline{}
	if err = json.Unmarshal(expandedRaw, &p); err != nil {
		return p, Version{}, &PathError{"$", fmt.Errorf("invalid expanded pipeline")}
	}
	for i, st := range p.Stages {
		if !work.step(1) {
			return p, Version{}, work.err
		}
		if st.OnGateFailure == "" {
			return p, Version{}, &RuleError{&PathError{fmt.Sprintf("stages[%d].onGateFailure", i), fmt.Errorf("must be keep or rollback")}}
		}
	}
	v := Version{}
	if v.Source, err = withFormat(raw); err != nil {
		return p, v, err
	}
	if v.Expanded, err = json.Marshal(expandedDoc{VersionFormat, p.Owner, p.Source, p.Stages}); err != nil {
		return p, v, err
	}
	// Walk the stored shape so omitted/removed fields have no provenance.
	work.output = int64(len(v.Expanded) + 32)
	prov := map[string]any{"v": VersionFormat}
	var stored map[string]any
	if err = decodeNumbers(v.Expanded, &stored); err != nil {
		return p, v, err
	}
	var walk func(any, string)
	walk = func(value any, ptr string) {
		if !work.step(int64(len(ptr) + 1)) {
			return
		}
		switch x := value.(type) {
		case map[string]any:
			if len(x) > 0 {
				for k, c := range x {
					if !work.step(int64(len(ptr) + 2*len(k) + 1)) {
						return
					}
					walk(c, ptr+"/"+pointerToken(k))
				}
				return
			}
		case []any:
			if ptr == "/stages" || ptr == "/source/images" {
				for i, c := range x {
					if !work.step(int64(len(ptr) + 24)) {
						return
					}
					walk(c, fmt.Sprintf("%s/%d", ptr, i))
				}
				return
			}
		}
		if ptr == "/v" {
			return
		}
		origin, ok := origins[ptr]
		if !ok {
			for parent := ptr; parent != ""; {
				if !work.step(1) {
					return
				}
				j := strings.LastIndex(parent, "/")
				parent = parent[:j]
				if o, found := origins[parent]; found {
					origin = o
					ok = true
					break
				}
			}
		}
		if !ok {
			path := strings.TrimPrefix(strings.ReplaceAll(ptr, "/", "."), ".")
			for i, st := range p.Stages {
				if !work.step(1) {
					return
				}
				prefix := fmt.Sprintf("/stages/%d", i)
				if ptr == prefix || strings.HasPrefix(ptr, prefix+"/") {
					path = "stages[" + st.Name + "]" + strings.ReplaceAll(strings.TrimPrefix(ptr, prefix), "/", ".")
					break
				}
			}
			for i := range p.Source.Images {
				if !work.step(1) {
					return
				}
				prefix := fmt.Sprintf("/source/images/%d", i)
				if ptr == prefix || strings.HasPrefix(ptr, prefix+"/") {
					path = fmt.Sprintf("source.images[%d]", i) + strings.ReplaceAll(strings.TrimPrefix(ptr, prefix), "/", ".")
					break
				}
			}
			origin = Origin{From: FromProject, Path: path}
		}
		if !work.step(int64(6*(len(ptr)+len(origin.From)+len(origin.Path)) + 64)) {
			return
		}
		if !work.reserveOutput(jsonStringSize(ptr) + jsonStringSize(origin.From) + jsonStringSize(origin.Path) + 32) {
			return
		}
		prov[ptr] = origin
	}
	walk(stored, "")
	if !work.step(1) {
		return p, Version{}, work.err
	}
	if v.Provenance, err = json.Marshal(prov); err != nil {
		return p, v, err
	}
	if len(v.Expanded)+len(v.Provenance) > maxExpansionBytes {
		return p, Version{}, ErrExpansionBudget
	}
	if !work.step(int64(len(v.Source) + len(v.Expanded) + len(v.Provenance))) {
		return p, Version{}, work.err
	}
	if validationErr := checkExpandedPatterns(p); validationErr != nil {
		explained := ExplainErrorsContext(ctx, p, v, platformVersion, []error{validationErr})
		if !work.step(1) {
			return Pipeline{}, Version{}, work.err
		}
		var located *PathError
		if errors.As(explained[0], &located) {
			return p, v, &RuleError{located}
		}
		return p, v, validationErr
	}
	v.Digest, err = digestContext(ctx, v.Source, v.Expanded, v.Provenance)
	if err != nil {
		return Pipeline{}, Version{}, err
	}
	return p, v, err
}

func recordOrigins(work *expansionWork, value any, ptr, path, from string, out map[string]Origin) {
	if !work.step(int64(len(ptr) + len(path) + len(from) + 1)) {
		return
	}
	if obj, ok := value.(map[string]any); ok && len(obj) > 0 {
		for k, v := range obj {
			if !work.step(int64(len(ptr) + len(path) + 3*len(k) + 2)) {
				return
			}
			recordOrigins(work, v, ptr+"/"+pointerToken(k), child(path, k), from, out)
		}
		return
	}
	if ptr == "/source/images" {
		if arr, ok := value.([]any); ok {
			for i, v := range arr {
				if !work.step(int64(len(ptr) + len(path) + 48)) {
					return
				}
				recordOrigins(work, v, fmt.Sprintf("%s/%d", ptr, i), fmt.Sprintf("%s[%d]", path, i), from, out)
			}
			return
		}
	}
	if from == FromBuiltin {
		path = ""
	}
	if !work.reserveOutput(jsonStringSize(ptr) + jsonStringSize(from) + jsonStringSize(path) + 32) {
		return
	}
	out[ptr] = Origin{From: from, Path: path}
}

func mergeLayer(work *expansionWork, dst, patch map[string]any, ptr, path, from string, origins map[string]Origin) {
	if !work.step(int64(len(ptr) + len(path) + 1)) {
		return
	}
	for _, k := range sortedKeys(patch) {
		if !work.step(1) {
			return
		}
		v := patch[k]
		// An empty project policy is absent: preserve inherited policy and provenance (§14.3).
		if k == "onGateFailure" && strings.Count(ptr, "/") == 2 && strings.HasPrefix(ptr, "/stages/") && from == FromProject && v == "" {
			continue
		}
		if !work.step(int64(len(ptr) + len(path) + 3*len(k) + 2)) {
			return
		}
		p := ptr + "/" + pointerToken(k)
		if v == nil {
			clearOrigins(work, dst[k], p, origins)
			delete(dst, k)
			continue
		}
		if obj, ok := v.(map[string]any); ok {
			base, ok := dst[k].(map[string]any)
			if !ok {
				clearOrigins(work, dst[k], p, origins)
				base = map[string]any{}
			}
			delete(origins, p)
			mergeLayer(work, base, obj, p, child(path, k), from, origins)
			dst[k] = base
			if len(base) == 0 {
				recordOrigins(work, base, p, child(path, k), from, origins)
			}
			continue
		}
		clearOrigins(work, dst[k], p, origins)
		dst[k] = v
		recordOrigins(work, v, p, child(path, k), from, origins)
	}
}

// clearOrigins visits only the replaced subtree; work is linear in layer size (CFG-4).
func clearOrigins(work *expansionWork, value any, ptr string, origins map[string]Origin) {
	if !work.step(int64(len(ptr) + 1)) {
		return
	}
	delete(origins, ptr)
	if obj, ok := value.(map[string]any); ok {
		for k, v := range obj {
			if !work.step(int64(len(ptr) + 3*len(k) + 1)) {
				return
			}
			clearOrigins(work, v, ptr+"/"+pointerToken(k), origins)
		}
	}
}

func decodeNumbers(raw []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	return d.Decode(dst)
}

func substitute(work *expansionWork, value any, project, stage, path string) error {
	if !work.step(int64(len(path) + 1)) {
		return work.err
	}
	replace := func(s, path string) (string, error) {
		if !work.step(int64(len(s))) {
			return "", work.err
		}
		var b strings.Builder
		for i := 0; i < len(s); {
			if !work.step(1) {
				return "", work.err
			}
			if strings.HasPrefix(s[i:], "$$") {
				if !work.reserveOutput(1) {
					return "", work.err
				}
				b.WriteByte('$')
				i += 2
				continue
			}
			if strings.HasPrefix(s[i:], "${") {
				end := strings.IndexByte(s[i:], '}')
				if end < 0 {
					return "", &PathError{substitutionErrorPath(path), fmt.Errorf("unknown placeholder")}
				}
				key := s[i+2 : i+end]
				switch key {
				case "project":
					if !work.reserveOutput(int64(len(project))) || !work.step(int64(len(project))) {
						return "", work.err
					}
					b.WriteString(project)
				case "stage":
					if stage == "" {
						return "", &PathError{substitutionErrorPath(path), fmt.Errorf("stage placeholder requires a stage")}
					}
					if !work.reserveOutput(int64(len(stage))) || !work.step(int64(len(stage))) {
						return "", work.err
					}
					b.WriteString(stage)
				default:
					return "", &PathError{substitutionErrorPath(path), fmt.Errorf("unknown placeholder")}
				}
				i += end + 1
				continue
			}
			if !work.reserveOutput(1) {
				return "", work.err
			}
			b.WriteByte(s[i])
			i++
		}
		return b.String(), nil
	}
	switch x := value.(type) {
	case map[string]any:
		for k, v := range x {
			if !work.step(int64(len(path) + len(k) + 1)) {
				return work.err
			}
			if k == "name" && strings.HasPrefix(path, "stages[") && !strings.Contains(path, ".") {
				continue
			}
			if s, ok := v.(string); ok {
				r, err := replace(s, child(path, k))
				if err != nil {
					return err
				}
				x[k] = r
			} else if err := substitute(work, v, project, stage, child(path, k)); err != nil {
				return err
			}
		}
	case []any:
		for i, v := range x {
			if !work.step(int64(len(path) + 24)) {
				return work.err
			}
			if s, ok := v.(string); ok {
				r, err := replace(s, fmt.Sprintf("%s[%d]", path, i))
				if err != nil {
					return err
				}
				x[i] = r
			} else if err := substitute(work, v, project, stage, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Opaque config keys can themselves contain secrets. Report known schema fields;
// otherwise locate the deploy config without echoing a submitted key (CFG-6).
func substitutionErrorPath(path string) string {
	if i := strings.Index(path, ".gate["); i >= 0 {
		end := strings.IndexByte(path[i:], ']')
		if end >= 0 {
			end += i + 1
			tail := path[end:]
			if tail != ".bake" && tail != ".approval" {
				return path[:end]
			}
		}
	}
	if i := strings.Index(path, ".deploy."); i >= 0 {
		tail := path[i+len(".deploy."):]
		if tail != "argocd.app" && tail != "argocd.appNamespace" {
			return path[:i] + ".deploy"
		}
	}
	return path
}

// Reserve an escaped JSON upper bound before serialization (CFG-4).
func chargeValue(w *expansionWork, v any, depth int) error {
	if depth > maxConfigDepth {
		return ErrExpansionBudget
	}
	if !w.reserveOutput(8) || !w.step(8) {
		return w.err
	}
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			if !w.reserveOutput(jsonStringSize(k)+2) || !w.step(int64(6*len(k)+4)) {
				return w.err
			}
			if err := chargeValue(w, c, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, c := range x {
			if err := chargeValue(w, c, depth+1); err != nil {
				return err
			}
		}
	case json.Number:
		if !w.reserveOutput(int64(len(x))) || !w.step(int64(len(x))) {
			return w.err
		}
	case string:
		if !w.reserveOutput(jsonStringSize(x)) || !w.step(int64(6*len(x))) {
			return w.err
		}
	}
	return nil
}
