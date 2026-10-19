package plugin

import (
	"fmt"
	"reflect"
	"sync"
)

// Registry resolves compiled plugins by name (docs/design/07-plugins.md §7.4).
// Its zero value is ready to use, and its methods are safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	deployers map[string]Deployer
	gateSteps map[string]GateStep
}

// NewRegistry creates an empty plugin registry.
func NewRegistry() *Registry { return &Registry{} }

// RegisterDeployer registers a deployer without replacing an existing registration.
func (r *Registry) RegisterDeployer(name string, d Deployer) error {
	if name == "" || isNil(d) {
		return fmt.Errorf("plugin: register deployer %q: name and implementation are required", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.deployers[name]; exists {
		return fmt.Errorf("plugin: deployer %q already registered", name)
	}
	if r.deployers == nil {
		r.deployers = make(map[string]Deployer)
	}
	r.deployers[name] = d
	return nil
}

// RegisterGateStep registers a gate step without replacing an existing registration.
func (r *Registry) RegisterGateStep(name string, g GateStep) error {
	if name == "" || isNil(g) {
		return fmt.Errorf("plugin: register gate step %q: name and implementation are required", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.gateSteps[name]; exists {
		return fmt.Errorf("plugin: gate step %q already registered", name)
	}
	if r.gateSteps == nil {
		r.gateSteps = make(map[string]GateStep)
	}
	r.gateSteps[name] = g
	return nil
}

// Deployer looks up a registered deployer.
func (r *Registry) Deployer(name string) (Deployer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.deployers[name]
	return d, ok
}

// GateStep looks up a registered gate step.
func (r *Registry) GateStep(name string) (GateStep, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.gateSteps[name]
	return g, ok
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}
