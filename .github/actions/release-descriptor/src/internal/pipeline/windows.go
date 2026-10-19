package pipeline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"
	_ "time/tzdata" // §12.1: resolve from the binary's embedded IANA database.

	"github.com/Vibrant-America/promotion-controller/internal/engine"
	yamlstream "go.yaml.in/yaml/v2"
	"sigs.k8s.io/yaml"
)

var windowZones sync.Map

// ResolveTimeZone is the process-wide location cache shared by config and store
// loaders (§12.2). Engine evaluation never calls it (D-033).
func ResolveTimeZone(name string) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, fmt.Errorf("timeZone must be an IANA name")
	}
	if loc, ok := windowZones.Load(name); ok {
		return loc.(*time.Location), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("resolve timeZone %q: %w", name, err)
	}
	cached, _ := windowZones.LoadOrStore(name, loc)
	return cached.(*time.Location), nil
}

// Platform is platform.yaml as applied through the API (D-034, D-035).
type Platform struct {
	APIVersion    string             `json:"apiVersion"`
	Notifications Notifications      `json:"notifications,omitempty,omitzero"`
	Windows       []WindowDefinition `json:"windows"`
	Templates     []Template         `json:"templates,omitempty"`
	Defaults      Defaults           `json:"defaults,omitempty,omitzero"`
}

// Notifications holds versioned platform notification defaults (§15.3).
type Notifications struct {
	Slack SlackDefaults `json:"slack,omitempty,omitzero"`
}

// SlackDefaults supplies the fallback when owner.slack is empty.
type SlackDefaults struct {
	DefaultChannel string `json:"defaultChannel,omitempty"`
}

// WindowDefinition is one platform window as configured (config doc §8.3). The store keeps its schedule in this
// shape, so a window reads back as it was applied (storage doc §5.1).
type WindowDefinition struct {
	Name      string               `json:"name"`
	Mode      engine.WindowMode    `json:"mode"`
	Recurring *RecurringDefinition `json:"recurring,omitempty"`
	Once      *OnceDefinition      `json:"once,omitempty"`
}

// RecurringDefinition is a recurring schedule as configured: day names, HH:MM times and an IANA zone name.
type RecurringDefinition struct {
	Days     []string `json:"days"`
	Start    string   `json:"start"`
	End      string   `json:"end"`
	TimeZone string   `json:"timeZone"`
}

// OnceDefinition is a one-off schedule as configured: RFC 3339 times with an offset.
type OnceDefinition struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// PlatformVersion is the content of an immutable platform version (docs/design/14-config-versions.md §14.5).
type PlatformVersion struct {
	Parsed json.RawMessage // the versioned decoded document, with templates sorted by name
	Digest string          // the version identity: hex sha256 of the canonical JSON of Parsed
}

type parsedPlatform struct {
	V             int                `json:"v"`
	Notifications Notifications      `json:"notifications,omitempty,omitzero"`
	Windows       []WindowDefinition `json:"windows"`
	Templates     []Template         `json:"templates,omitempty"`
	Defaults      Defaults           `json:"defaults,omitempty,omitzero"`
}

// NewPlatformVersion decodes doc (DecodePlatform) and returns the platform with its windows and its version content.
// Two files that differ only in comments, formatting or the order of their windows have the same version.
func NewPlatformVersion(doc []byte) (Platform, []engine.Window, PlatformVersion, error) {
	p, ws, err := DecodePlatform(doc)
	if err != nil {
		return p, ws, PlatformVersion{}, err
	}
	var v PlatformVersion
	if v.Parsed, err = json.Marshal(parsedPlatform{V: VersionFormat, Windows: p.Windows, Templates: p.Templates, Defaults: p.Defaults, Notifications: p.Notifications}); err != nil {
		return p, ws, v, fmt.Errorf("pipeline: platform: %w", err)
	}
	c, err := Canonical(v.Parsed)
	if err != nil {
		return p, ws, v, fmt.Errorf("pipeline: platform digest: %w", err)
	}
	sum := sha256.Sum256(c)
	v.Digest = hex.EncodeToString(sum[:])
	return p, ws, v, nil
}

// Windows strictly parses platform.yaml windows with YAML-path errors (§12.2), sorted by name.
func Windows(doc []byte) ([]engine.Window, error) {
	_, ws, err := DecodePlatform(doc)
	return ws, err
}

// RuleError marks a semantic configuration violation rather than a decoding failure.
type RuleError struct{ *PathError }

// Unwrap returns the PathError, so errors.As finds the path.
func (e *RuleError) Unwrap() error { return e.PathError }

// DecodePlatform strictly decodes platform.yaml and validates its windows (§12.2): names are unique DNS labels, and
// each window passes Window. Like Decode's, its errors are PathErrors that echo no input (principle 18). It returns the platform with its windows sorted by name, so reordering the file changes
// nothing, and the windows for the engine in the same order.
func DecodePlatform(doc []byte) (Platform, []engine.Window, error) {
	var parsed Platform
	fail := func(path string, err error) (Platform, []engine.Window, error) {
		return Platform{}, nil, &PathError{path, err}
	}
	if len(doc) > 1<<20 {
		return fail("$", fmt.Errorf("platform input exceeds 1 MiB limit (1048576 bytes)"))
	}
	if err := checkDecodeLimits(doc, "platform"); err != nil {
		return Platform{}, nil, err
	}
	stream := yamlstream.NewDecoder(bytes.NewReader(doc))
	stream.SetStrict(true)
	var value any
	if err := stream.Decode(&value); err != nil {
		return fail("$", yamlError(err))
	}
	if err := stream.Decode(&value); err != io.EOF {
		if err == nil {
			return fail("$", fmt.Errorf("decode YAML: expected exactly one YAML document"))
		}
		return fail("$", yamlError(err))
	}
	raw, err := yaml.YAMLToJSONStrict(doc)
	if err != nil {
		return fail("$", yamlError(err))
	}
	if err := checkShape(raw, reflect.TypeOf(parsed), ""); err != nil {
		return Platform{}, nil, err
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fail("$", fmt.Errorf("decode platform: a value has the wrong type"))
	}
	if parsed.APIVersion != APIVersion {
		return fail("apiVersion", fmt.Errorf("must be %q", APIVersion))
	}
	if ch := parsed.Notifications.Slack.DefaultChannel; ch != "" && !slackChannel.MatchString(ch) {
		return Platform{}, nil, &RuleError{&PathError{"notifications.slack.defaultChannel", fmt.Errorf("must be a channel name (#name) or channel id (C…) of at most 80 characters")}}
	}
	if err := checkTemplates(parsed); err != nil {
		return Platform{}, nil, err
	}
	sort.Slice(parsed.Templates, func(i, j int) bool { return parsed.Templates[i].Name < parsed.Templates[j].Name })
	out := []engine.Window{}
	names := map[string]bool{}
	breaks := func(path string, err error) (Platform, []engine.Window, error) {
		return Platform{}, nil, &RuleError{&PathError{path, err}}
	}
	for i, d := range parsed.Windows {
		path := fmt.Sprintf("windows[%d]", i)
		if len(d.Name) > 63 || !dnsLabel.MatchString(d.Name) {
			return breaks(path+".name", fmt.Errorf("must be a DNS label of 1–63 characters"))
		}
		if names[d.Name] {
			return breaks(path+".name", fmt.Errorf("duplicate window name"))
		}
		names[d.Name] = true
		w, err := d.window(path)
		if err != nil {
			var pe *PathError
			if errors.As(err, &pe) {
				return Platform{}, nil, &RuleError{pe}
			}
			return Platform{}, nil, err
		}
		out = append(out, w)
	}
	if parsed.Windows == nil {
		parsed.Windows = []WindowDefinition{}
	}
	sort.Slice(parsed.Windows, func(i, j int) bool { return parsed.Windows[i].Name < parsed.Windows[j].Name })
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return parsed, out, nil
}

// Window turns the definition into the engine's window, resolving its time zone through the process-wide cache
// (§12.2). Errors name the window.
func (d WindowDefinition) Window() (engine.Window, error) { return d.window("window " + d.Name) }

func (d WindowDefinition) window(path string) (engine.Window, error) {
	fail := func(path string, err error) (engine.Window, error) { return engine.Window{}, &PathError{path, err} }
	if d.Mode != engine.WindowAllow && d.Mode != engine.WindowDeny {
		return fail(path+".mode", fmt.Errorf("must be allow or deny"))
	}
	if (d.Recurring == nil) == (d.Once == nil) {
		return fail(path, fmt.Errorf("exactly one of recurring and once is required"))
	}
	w := engine.Window{Name: d.Name, Mode: d.Mode}
	if r := d.Recurring; r != nil {
		if len(r.Days) == 0 {
			return fail(path+".recurring.days", fmt.Errorf("must not be empty"))
		}
		days := []time.Weekday{}
		for j, name := range r.Days {
			day := -1
			for k, n := range []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"} {
				if name == n {
					day = k
				}
			}
			if day < 0 {
				return fail(fmt.Sprintf("%s.recurring.days[%d]", path, j), fmt.Errorf("unknown day (allowed: Sun, Mon, Tue, Wed, Thu, Fri, Sat)"))
			}
			for _, old := range days {
				if old == time.Weekday(day) {
					return fail(fmt.Sprintf("%s.recurring.days[%d]", path, j), fmt.Errorf("duplicate day"))
				}
			}
			days = append(days, time.Weekday(day))
		}
		start, err := windowMinutes(r.Start, false)
		if err != nil {
			return fail(path+".recurring.start", err)
		}
		end, err := windowMinutes(r.End, true)
		if err != nil {
			return fail(path+".recurring.end", err)
		}
		if start == end {
			return fail(path+".recurring.end", fmt.Errorf("must differ from start"))
		}
		loc, err := ResolveTimeZone(r.TimeZone)
		if err != nil {
			return fail(path+".recurring.timeZone", fmt.Errorf("must be a known IANA time zone name"))
		}
		w.Recurring = &engine.Recurring{Days: days, Start: start, End: end, TimeZone: r.TimeZone, Loc: loc}
	} else {
		from, err := time.Parse(time.RFC3339, d.Once.From)
		if err != nil {
			return fail(path+".once.from", fmt.Errorf("must be RFC 3339 with an offset"))
		}
		to, err := time.Parse(time.RFC3339, d.Once.To)
		if err != nil {
			return fail(path+".once.to", fmt.Errorf("must be RFC 3339 with an offset"))
		}
		if !to.After(from) {
			return fail(path+".once.to", fmt.Errorf("must be after from"))
		}
		w.Once = &engine.OneOff{From: from, To: to}
	}
	if err := w.Validate(); err != nil {
		return fail(path, err)
	}
	return w, nil
}

func windowMinutes(s string, end bool) (int, error) {
	invalid := func() (int, error) { return 0, fmt.Errorf("must be HH:MM") }
	if len(s) != 5 || s[2] != ':' {
		return invalid()
	}
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return invalid()
		}
	}
	hour, _ := strconv.Atoi(s[:2])
	minute, _ := strconv.Atoi(s[3:])
	if end && hour == 24 && minute == 0 {
		return 1440, nil
	}
	if hour > 23 || minute > 59 {
		return invalid()
	}
	return hour*60 + minute, nil
}
