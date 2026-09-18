package backend

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type FactoryConfig struct {
	ProjectDirectory string
	StateDirectory   string
	SessionID        string
	AuthConfigPath   string
	Settings         map[string]any
}

type Factory func(context.Context, FactoryConfig) (Backend, error)

type Registry struct{ factories map[string]Factory }

func NewRegistry() *Registry { return &Registry{factories: map[string]Factory{}} }

func (r *Registry) Register(name string, factory Factory) error {
	name = strings.TrimSpace(name)
	if name == "" || factory == nil {
		return fmt.Errorf("backend name and factory are required")
	}
	if _, exists := r.factories[name]; exists {
		return fmt.Errorf("backend %q is already registered", name)
	}
	r.factories[name] = factory
	return nil
}

func (r *Registry) Build(ctx context.Context, name string, config FactoryConfig) (Backend, error) {
	factory, ok := r.factories[strings.TrimSpace(name)]
	if !ok {
		return nil, fmt.Errorf("backend %q is not registered", name)
	}
	result, err := factory(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("build backend %q: %w", name, err)
	}
	if result == nil {
		return nil, fmt.Errorf("backend %q factory returned nil", name)
	}
	return result, nil
}

func (r *Registry) Names() []string {
	result := make([]string, 0, len(r.factories))
	for name := range r.factories {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}
