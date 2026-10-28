package schema

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistry_RegisterFindAndValidate(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	schema := Schema{
		"properties": map[string]any{
			"firstName": map[string]any{"type": "string", "required": true},
			"age":       map[string]any{"type": "number"},
			"isActive":  map[string]any{"type": "boolean"},
		},
	}
	if err := registry.Register("io.genesisdb.app.customer-added", schema); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("io.genesisdb.app.customer-added", schema); !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("got %v", err)
	}
	if err := registry.Register("x", Schema{}); !errors.Is(err, ErrInvalidSchema) {
		t.Fatalf("got %v", err)
	}
	cases := map[string]string{
		`{"firstName":"Bruce","age":40,"isActive":true}`: "",
		`{"age":40}`:                        "required field 'firstName' is missing",
		`{"firstName":1}`:                   "must be a string",
		`{"firstName":"Bruce","age":"old"}`: "must be a number",
		`{"firstName":"Bruce","extra":1}`:   "is not allowed by schema",
		`{"firstName":null}`:                "cannot be null",
	}
	for data, want := range cases {
		err := registry.ValidateEvent("io.genesisdb.app.customer-added", json.RawMessage(data))
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: got %v, want %q", data, err, want)
		}
	}
	if err := registry.ValidateEvent("unregistered", json.RawMessage(`{"anything":1}`)); err != nil {
		t.Fatal("types without a schema must be valid")
	}
	listed, _ := registry.List()
	if len(listed) != 1 || listed[0].EventType != "io.genesisdb.app.customer-added" {
		t.Fatalf("listed %+v", listed)
	}
}

func TestSchemaFileName_MatchesEarlierReleases(t *testing.T) {
	cases := map[string]string{
		"io.genesisdb.app.customer-added": "io_genesisdb_app_customer-added.json",
		"1st":                             "schema_1st.json",
		"_x":                              "schema__x.json",
	}
	for eventType, want := range cases {
		if got := schemaFileName(eventType); got != want {
			t.Errorf("%s: %s want %s", eventType, got, want)
		}
	}
}

// Schema files written by GenesisDB 1.x (sanitized file name, bare
// definition, lenient keywords) are migrated once into the current envelope
// and keep validating exactly what 1.x validated.
func TestMigrateLegacyFiles(t *testing.T) {
	directory := t.TempDir()
	legacy := `{"allowAdditionalProperties":true,"properties":{"age":{"type":"number","minimum":0},"firstName":{"required":true,"type":"string","pattern":"ignored by 1.x"}},"$schema":"ignored"}`
	if err := os.WriteFile(filepath.Join(directory, "io_genesisdb_app_customer-added.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(directory)
	migrated, err := registry.MigrateLegacyFiles()
	if err != nil || len(migrated) != 1 || migrated[0] != "io.genesisdb.app.customer-added" {
		t.Fatalf("migrated %v %v", migrated, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "io_genesisdb_app_customer-added.json")); !os.IsNotExist(err) {
		t.Fatal("legacy file not removed")
	}
	if _, err := os.Stat(filepath.Join(directory, currentFileName("io.genesisdb.app.customer-added"))); err != nil {
		t.Fatal("current file not created")
	}
	if again, err := registry.MigrateLegacyFiles(); err != nil || len(again) != 0 {
		t.Fatalf("second run is not a no-op: %v %v", again, err)
	}
	for data, wantError := range map[string]bool{
		`{"firstName":"Bruce","age":40,"extra":true}`: false,
		`{"age":40}`:                    true,
		`{"firstName":1}`:               true,
		`{"firstName":"x","age":"old"}`: true,
	} {
		err := registry.ValidateEvent("io.genesisdb.app.customer-added", json.RawMessage(data))
		if (err != nil) != wantError {
			t.Errorf("%s: %v", data, err)
		}
	}
	listed, err := registry.List()
	if err != nil || len(listed) != 1 || listed[0].EventType != "io.genesisdb.app.customer-added" {
		t.Fatalf("list after migration: %v %v", listed, err)
	}
	if _, hasPattern := listed[0].Schema["properties"].(map[string]any)["firstName"].(map[string]any)["pattern"]; hasPattern {
		t.Fatal("keyword 1.x ignored survived the migration")
	}
}
