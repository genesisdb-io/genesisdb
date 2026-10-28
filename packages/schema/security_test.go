package schema

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSchemaIdentityDoesNotCollide(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	for _, name := range []string{"a.b", "a_b", "a/b"} {
		definition := Schema{"properties": map[string]any{name: map[string]any{"type": "string"}}}
		if err := registry.Register(name, definition); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := registry.ValidateEvent(name, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := registry.List()
	if err != nil || len(listed) != 3 {
		t.Fatalf("identity collision: %+v %v", listed, err)
	}
}

func TestConcurrentRegistrationPublishesExactlyOnce(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	var group sync.WaitGroup
	var successes atomic.Int32
	for position := 0; position < 16; position++ {
		group.Add(1)
		go func() {
			defer group.Done()
			err := registry.Register("created", Schema{"properties": map[string]any{}})
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrAlreadyRegistered) {
				t.Errorf("registration: %v", err)
			}
		}()
	}
	group.Wait()
	if successes.Load() != 1 {
		t.Fatalf("%d registrations succeeded", successes.Load())
	}
	if _, err := registry.Find("created"); err != nil {
		t.Fatal(err)
	}
}

func TestStrictSchemaSubset(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	for _, definition := range []Schema{
		{"type": "typo"}, {"properties": "invalid"}, {"type": "string", "pattern": "ignored"},
		{"properties": map[string]any{"x": map[string]any{"type": "unknown"}}},
	} {
		if err := registry.Register("invalid", definition); !errors.Is(err, ErrInvalidSchema) {
			t.Fatalf("unsupported schema accepted: %v", err)
		}
	}
	definition := Schema{"type": "object", "properties": map[string]any{
		"items": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
	}, "required": []any{"items"}, "additionalProperties": false}
	if err := registry.Register("nested", definition); err != nil {
		t.Fatal(err)
	}
	if err := registry.ValidateEvent("nested", json.RawMessage(`{"items":[1,2]}`)); err != nil {
		t.Fatal(err)
	}
	if err := registry.ValidateEvent("nested", json.RawMessage(`{"items":[1,2.5]}`)); err == nil {
		t.Fatal("invalid nested item accepted")
	}
}
