package tool

import (
	"context"
	"testing"

	"zende.local/harness/internal/model"
)

type registeredTest struct{ descriptor Descriptor }

func (e registeredTest) Descriptor() Descriptor                        { return e.descriptor }
func (e registeredTest) Execute(context.Context, Call) (Result, error) { return Result{}, nil }

func TestRegistryOwnsDescriptorsAndRejectsInvalidDefinitions(t *testing.T) {
	d := Descriptor{Tool: model.Tool{Name: "example", InputSchema: []byte(`{"type":"object"}`)}, Version: "1", Guidelines: []string{"original"}}
	r, err := NewRegistry(registeredTest{d})
	if err != nil {
		t.Fatal(err)
	}
	d.InputSchema[0] = 'x'
	d.Guidelines[0] = "changed"
	got := r.Descriptors()
	if got[0].InputSchema[0] != '{' || got[0].Guidelines[0] != "original" {
		t.Fatal(got)
	}
	got[0].Guidelines[0] = "changed again"
	_, snapshot, ok := r.Lookup("example")
	if !ok || snapshot.Guidelines[0] != "original" {
		t.Fatal(snapshot)
	}
	if _, err := NewRegistry(registeredTest{snapshot}, registeredTest{snapshot}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := NewRegistry(registeredTest{Descriptor{}}); err == nil {
		t.Fatal("invalid descriptor accepted")
	}
	if _, err := NewRegistry(nil); err == nil {
		t.Fatal("nil executor accepted")
	}
}
