package backend

import (
	"context"
	"testing"
)

type registryBackend struct{ name string }

func (b registryBackend) Name() string { return b.name }
func (registryBackend) RunSubmission(context.Context, string, SubmissionRequest) (*SubmissionResult, error) {
	return &SubmissionResult{}, nil
}
func (registryBackend) CancelSubmission(context.Context, string, map[string]any) error { return nil }
func (registryBackend) Cancel(context.Context) error                                   { return nil }

func TestRegistryRegistersBuildsAndListsFactories(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("colab", func(context.Context, FactoryConfig) (Backend, error) { return registryBackend{name: "colab"}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.Register("local", func(context.Context, FactoryConfig) (Backend, error) { return registryBackend{name: "local"}, nil }); err != nil {
		t.Fatal(err)
	}
	if got := r.Names(); len(got) != 2 || got[0] != "colab" || got[1] != "local" {
		t.Fatalf("unexpected names: %#v", got)
	}
	b, err := r.Build(context.Background(), "colab", FactoryConfig{SessionID: "gpu"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "colab" {
		t.Fatalf("unexpected backend: %s", b.Name())
	}
	if err := r.Register("colab", func(context.Context, FactoryConfig) (Backend, error) { return nil, nil }); err == nil {
		t.Fatal("expected duplicate registration error")
	}
}
