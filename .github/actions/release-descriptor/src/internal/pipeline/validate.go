package pipeline

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"

	"github.com/Vibrant-America/promotion-controller/internal/descriptor"
	"github.com/Vibrant-America/promotion-controller/internal/github"
	"github.com/Vibrant-America/promotion-controller/internal/plugin"
)

// ValidationContext supplies platform and persisted state for §8.2 validation.
// TakenKeys maps deployer exclusive keys to the stages that hold them; the current project's
// keys may be reused on re-apply. Nil sets are empty (including phase 0 windows).
// AllowRemove and Busy are only for CheckRemovals: Busy maps a stage with an active deployment to what it is doing,
// such as "prod-eu-3: v0.2.9 deploying".
type ValidationContext struct {
	GitHubWebURL string
	Windows      map[string]bool
	TakenKeys    map[string]KeyHolder
	Previous     *Pipeline
	AllowRemove  bool
	Busy         map[string]string
}

// KeyHolder is the stage that holds a deployer exclusive key.
type KeyHolder struct {
	Project string
	Stage   string
}

// ErrKeyTaken is the error of a deployer exclusive key that another project's stage holds. An apply whose only
// errors are these answers 409, not 422 (docs/design/14-config-versions.md §14.2).
var ErrKeyTaken = errors.New("exclusive key belongs to another project")

// KeyTakenError is the §8.2 error of a deployer config whose exclusive key the holder's stage holds. It names the
// holder by its stored names, never by the key, which is input.
func KeyTakenError(path string, holder KeyHolder) error {
	return &PathError{path, keyTaken(holder)}
}

type keyTaken KeyHolder

func (k keyTaken) Error() string {
	return fmt.Sprintf("exclusive key belongs to stage %q of project %q", k.Stage, k.Project)
}
func (keyTaken) Is(target error) bool { return target == ErrKeyTaken }

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Slack destinations are optional: unset uses the platform channel or log fallback (§15.3).
var slackChannel = regexp.MustCompile(`^(#[a-z0-9_-]{1,80}|C[A-Z0-9]{1,79})$`)

// Validate checks all pipeline rules in docs/design/08-config.md §8.2.
// It does not mutate p, call plugin runtime methods, or perform I/O. Its errors are PathErrors that echo nothing of
// p: a value or an unknown key is named by its path only. Previous is the stored pipeline, and the stage and project
// that hold a key are stored names, so they are shown. Stage removals are CheckRemovals'.
func Validate(p Pipeline, reg *plugin.Registry, ctx ValidationContext) []error {
	var errs []error
	add := func(path, format string, args ...any) {
		errs = append(errs, &PathError{path, fmt.Errorf(format, args...)})
	}
	name := func(path, value string) {
		if len(value) > 63 || !dnsLabel.MatchString(value) {
			add(path, "must be a DNS label of 1–63 characters")
		}
	}
	if p.APIVersion != APIVersion {
		add("apiVersion", "must be %q", APIVersion)
	}
	name("project", p.Project)
	if p.Owner.Slack != "" && !slackChannel.MatchString(p.Owner.Slack) {
		add("owner.slack", "must be a channel name (#name) or channel id (C…)")
	}
	errs = append(errs, ValidateSource(p.Source, ctx.GitHubWebURL)...)
	stages := map[string]int{}
	roots := 0
	for i, s := range p.Stages {
		path := fmt.Sprintf("stages[%d]", i)
		name(path+".name", s.Name)
		if _, exists := stages[s.Name]; exists {
			add(path+".name", "duplicate stage name")
		} else {
			stages[s.Name] = i
		}
		if s.Upstream == "" {
			roots++
		}
	}
	// D-008: one root and one upstream per other stage, with fan-out allowed.
	if roots != 1 {
		add("stages", "exactly one stage must have no upstream (got %d)", roots)
	}
	keys := map[string]string{}
	for i, s := range p.Stages {
		path := fmt.Sprintf("stages[%d]", i)
		if s.Upstream != "" {
			if _, ok := stages[s.Upstream]; !ok {
				add(path+".upstream", "unknown stage")
			}
		}
		if len(s.Deploy) != 1 {
			add(path+".deploy", "must contain exactly one deployer plugin")
		}
		for _, kind := range sortedKeys(s.Deploy) {
			cfg := s.Deploy[kind]
			var d plugin.Deployer
			var ok bool
			if reg != nil {
				d, ok = reg.Deployer(kind)
			}
			if !ok {
				add(path+".deploy", "unknown deployer plugin") // the unknown name is input
				continue
			}
			pluginPath := path + ".deploy." + kind
			if err := d.Validate(cfg); err != nil {
				add(pluginPath, "invalid config: %w", err)
				continue
			}
			key := d.ExclusiveKey(cfg)
			if earlier, ok := keys[key]; ok {
				add(pluginPath, "exclusive key already used by %s", earlier)
			} else {
				keys[key] = pluginPath
			}
			if holder, ok := ctx.TakenKeys[key]; ok && holder.Project != p.Project {
				errs = append(errs, KeyTakenError(pluginPath, holder))
			}
		}
		for j, step := range s.Gate {
			stepPath := fmt.Sprintf("%s.gate[%d]", path, j)
			if len(step) != 1 {
				add(stepPath, "must contain exactly one gate step type")
			}
			for _, kind := range sortedKeys(step) {
				// D-004: bake and approval are the only current gate step types.
				var g plugin.GateStep
				var ok bool
				if reg != nil {
					g, ok = reg.GateStep(kind)
				}
				if (kind != "bake" && kind != "approval") || !ok {
					add(stepPath, "unknown gate step type") // the unknown name is input
					continue
				}
				if err := g.Validate(step[kind]); err != nil {
					add(stepPath+"."+kind, "invalid config: %w", err)
				}
			}
		}
		if s.OnGateFailure != "" && s.OnGateFailure != "keep" && s.OnGateFailure != "rollback" {
			add(path+".onGateFailure", "must be keep or rollback")
		}
		for j, w := range s.Windows {
			if len(ctx.Windows) == 0 {
				add(fmt.Sprintf("%s.windows[%d]", path, j), "unknown window (no windows are configured)")
			} else if !ctx.Windows[w] {
				add(fmt.Sprintf("%s.windows[%d]", path, j), "unknown window")
			}
		}
	}
	// A color walk catches cycles even in disconnected components, independently
	// of root counting. Each stage's upstream edge is visited at most once.
	colors := make([]uint8, len(p.Stages))
	var visit func(int)
	visit = func(i int) {
		colors[i] = 1
		s := p.Stages[i]
		if next, ok := stages[s.Upstream]; s.Upstream != "" && ok {
			switch colors[next] {
			case 1:
				add(fmt.Sprintf("stages[%d].upstream", i), "cycle detected")
			case 0:
				visit(next)
			}
		}
		colors[i] = 2
	}
	for i := range p.Stages {
		if colors[i] == 0 {
			visit(i)
		}
	}
	if ctx.Previous != nil && ctx.Previous.Project != p.Project {
		add("project", "cannot change project identity from %q", ctx.Previous.Project)
	}
	return errs
}

// ValidateSource checks the source rules of §8.2 and the descriptor-mode rules of §8.8 (D-072). It is the one source
// validator shared by apply and the release-descriptor publisher (docs/design/23-release-descriptors.md §23.4). Like
// Validate, it does no I/O and its errors echo nothing of src.
func ValidateSource(src Source, githubWebURL string) []error {
	var errs []error
	add := func(path, format string, args ...any) {
		errs = append(errs, &PathError{path, fmt.Errorf(format, args...)})
	}
	if strings.TrimSpace(src.Repo) == "" {
		add("source.repo", "is required")
	} else if _, err := github.Repository(src.Repo, githubWebURL); err != nil {
		add("source.repo", "only GitHub repositories are supported")
	}
	if strings.TrimSpace(src.Branch) == "" {
		add("source.branch", "is required")
	}
	if len(src.Images) == 0 {
		add("source.images", "must contain at least one image")
	}
	images := map[string]bool{}
	for i, img := range src.Images {
		path := fmt.Sprintf("source.images[%d]", i)
		if strings.TrimSpace(img.Image) == "" {
			add(path+".image", "is required")
		}
		if images[img.Image] {
			add(path+".image", "duplicate image")
		}
		images[img.Image] = true
		pattern := img.TagPattern
		if pattern == "" {
			pattern = DefaultTagPattern
		}
		if _, err := regexp.Compile(pattern); err != nil {
			add(path+".tagPattern", "invalid regular expression%s", regexpProblem(err))
		}
	}
	return append(errs, validateDescriptorSource(src)...)
}

// validateDescriptorSource checks §8.8: descriptor mode is all or nothing. With source.descriptor, every image names
// its registry binding, the image set and names follow 23 §23.1, and every image is on one registry host, which is
// also the descriptor repository's host in v1. Without it, no image may name a registry.
func validateDescriptorSource(src Source) []error {
	var errs []error
	add := func(path, format string) { errs = append(errs, &PathError{path, errors.New(format)}) }
	d := src.Descriptor
	if d == nil {
		for i, img := range src.Images {
			if img.Registry != "" {
				add(fmt.Sprintf("source.images[%d].registry", i), "requires source.descriptor")
			}
		}
		return errs
	}
	if !descriptor.DNSLabel(d.Registry) {
		add("source.descriptor.registry", "must be a registry binding name: a DNS label of 1–63 characters")
	}
	if !descriptor.RepositoryPath(d.Repository) {
		add("source.descriptor.repository", "must be a lowercase OCI repository path of at most 255 characters")
	}
	if len(src.Images) > descriptor.MaxImages {
		add("source.images", fmt.Sprintf("must contain at most %d images with source.descriptor", descriptor.MaxImages))
	}
	hosts := map[string]bool{}
	for i, img := range src.Images {
		path := fmt.Sprintf("source.images[%d]", i)
		if !descriptor.DNSLabel(img.Registry) {
			add(path+".registry", "must be a registry binding name: a DNS label of 1–63 characters")
		}
		host, _, ok := descriptor.SplitImage(img.Image)
		if !ok {
			add(path+".image", "must be a registry host and a lowercase OCI repository path, with no tag or digest")
			continue
		}
		hosts[host] = true
	}
	if len(hosts) > 1 {
		add("source.images", "must all be on one registry host with source.descriptor")
	}
	return errs
}

// CheckRemovals checks the stages that p removes from ctx.Previous (D-037): removing one needs AllowRemove, and is
// refused while the stage is busy. An apply checks them after validation, under the project lock, and answers 409
// with these errors (docs/design/14-config-versions.md §14.2).
func CheckRemovals(p Pipeline, ctx ValidationContext) []error {
	if ctx.Previous == nil {
		return nil
	}
	kept := map[string]bool{}
	for _, s := range p.Stages {
		kept[s.Name] = true
	}
	var errs []error
	for _, old := range ctx.Previous.Stages {
		if kept[old.Name] {
			continue
		}
		// The removed stage has no index in the replacement document (§8.2).
		if !ctx.AllowRemove {
			errs = append(errs, &PathError{"stages", fmt.Errorf("removing stage %q needs --allow-remove", old.Name)})
		}
		if busy, ok := ctx.Busy[old.Name]; ok { // principle 4: say what to do
			errs = append(errs, &PathError{"stages", fmt.Errorf("stage %q is busy (%s); abort it or wait for it to finish, then apply again", old.Name, busy)})
		}
	}
	return errs
}

// regexpProblem returns ": " and the kind of a regexp syntax error, such as "missing closing ]", without the expression
// that the error quotes; or "" for any other error.
func regexpProblem(err error) string {
	var se *syntax.Error
	if errors.As(err, &se) {
		return ": " + string(se.Code)
	}
	return ""
}

// checkExpandedPatterns distinguishes explicit empty patterns from absence after expansion has filled defaults.
// The unexpanded compatibility validator cannot distinguish them in the string-only Image schema.
func checkExpandedPatterns(p Pipeline) error {
	for i, img := range p.Source.Images {
		if img.TagPattern == "" {
			return &RuleError{&PathError{fmt.Sprintf("source.images[%d].tagPattern", i), fmt.Errorf("must not be empty; omit it to use the default")}}
		}
	}
	return nil
}
