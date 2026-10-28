// Package schema implements a documented, strict JSON Schema subset, not
// a complete JSON Schema draft. Unsupported keywords are rejected.
package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Registry struct{ directory string }

func NewRegistry(directory string) *Registry { return &Registry{directory: directory} }

type Schema = map[string]any
type RegisteredSchema struct {
	EventType string
	Schema    Schema
}

var ErrAlreadyRegistered = errors.New("schema for this type already exists")
var ErrInvalidSchema = errors.New("invalid or unsupported schema")
var unsafeFileNameCharacters = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// schemaFileName returns the lossy file name GenesisDB 1.x used for a type.
// It is only used to read and migrate historical files.
func schemaFileName(eventType string) string {
	safe := unsafeFileNameCharacters.ReplaceAllString(eventType, "_")
	if len(safe) > 0 && ((safe[0] >= '0' && safe[0] <= '9') || safe[0] == '_') {
		safe = "schema_" + safe
	}
	return safe + ".json"
}
func eventTypeFromSchemaFileName(name string) string {
	return strings.ReplaceAll(strings.TrimSuffix(name, ".json"), "_", ".")
}
func currentFileName(eventType string) string {
	sum := sha256.Sum256([]byte(eventType))
	return "type-" + hex.EncodeToString(sum[:]) + ".json"
}

type schemaDocument struct {
	EventType string `json:"eventType"`
	Schema    Schema `json:"schema"`
}

// Register validates before publishing by an exclusive hard link. Readers
// never observe a partial file and concurrent registrations cannot overwrite.
func (registry *Registry) Register(eventType string, definition Schema) error {
	if strings.TrimSpace(eventType) == "" || len(eventType) > 1024 {
		return ErrInvalidSchema
	}
	if err := validateDefinition(definition, 0); err != nil {
		return err
	}
	existing, err := registry.Find(eventType)
	if err != nil {
		return err
	}
	if existing != nil {
		return ErrAlreadyRegistered
	}
	return registry.publish(eventType, definition)
}

// publish durably creates the current file of a type. It fails with
// ErrAlreadyRegistered if that file exists.
func (registry *Registry) publish(eventType string, definition Schema) error {
	encoded, err := json.Marshal(schemaDocument{eventType, definition})
	if err != nil {
		return err
	}
	if len(encoded) > 1024*1024 {
		return fmt.Errorf("%w: schema exceeds 1 MiB", ErrInvalidSchema)
	}
	if err := os.MkdirAll(registry.directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(registry.directory, ".schema-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err = temporary.Write(encoded); err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Link(temporary.Name(), filepath.Join(registry.directory, currentFileName(eventType))); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrAlreadyRegistered
		}
		return err
	}
	directory, err := os.Open(registry.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func readDocument(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return nil, fmt.Errorf("invalid schema file %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, 1024*1024+1))
}

func (registry *Registry) Find(eventType string) (Schema, error) {
	content, err := readDocument(filepath.Join(registry.directory, currentFileName(eventType)))
	if err == nil {
		var document schemaDocument
		if err := json.Unmarshal(content, &document); err != nil {
			return nil, err
		}
		if document.EventType != eventType {
			return nil, fmt.Errorf("schema identity mismatch")
		}
		return document.Schema, validateDefinition(document.Schema, 0)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	legacyName := schemaFileName(eventType)
	content, err = readDocument(filepath.Join(registry.directory, legacyName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var definition Schema
	if err := json.Unmarshal(content, &definition); err != nil {
		return nil, err
	}
	definition = normalizeLegacyDefinition(definition, 0)
	return definition, validateDefinition(definition, 0)
}

// MigrateLegacyFiles rewrites every schema file of GenesisDB 1.x into the
// current envelope with an exact type identity. 1.x stored schemas as
// <sanitized type>.json without the type itself, so the type is recovered
// from the file name (every character outside [a-zA-Z0-9_-] became "_", in
// practice "." in reverse DNS type names). Keywords 1.x ignored are dropped,
// so a schema that validated events in 1.x validates the same events here.
// The legacy file is removed only after the current file is durable. Running
// the migration again is a no-op. It returns the migrated event types.
func (registry *Registry) MigrateLegacyFiles() ([]string, error) {
	files, err := os.ReadDir(registry.directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var migrated []string
	for _, file := range files {
		name := file.Name()
		if !strings.HasSuffix(name, ".json") || isCurrentFileName(name) || !file.Type().IsRegular() {
			continue
		}
		content, err := readDocument(filepath.Join(registry.directory, name))
		if err != nil {
			return migrated, err
		}
		var definition Schema
		if err := json.Unmarshal(content, &definition); err != nil {
			return migrated, fmt.Errorf("legacy schema %s: %w", name, err)
		}
		definition = normalizeLegacyDefinition(definition, 0)
		if err := validateDefinition(definition, 0); err != nil {
			return migrated, fmt.Errorf("legacy schema %s: %w", name, err)
		}
		eventType := eventTypeFromSchemaFileName(name)
		switch err := registry.publish(eventType, definition); {
		case err == nil, errors.Is(err, ErrAlreadyRegistered):
		default:
			return migrated, fmt.Errorf("legacy schema %s: %w", name, err)
		}
		if err := os.Remove(filepath.Join(registry.directory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return migrated, err
		}
		migrated = append(migrated, eventType)
	}
	if len(migrated) > 0 {
		directory, err := os.Open(registry.directory)
		if err != nil {
			return migrated, err
		}
		defer directory.Close()
		return migrated, directory.Sync()
	}
	return migrated, nil
}

func isCurrentFileName(name string) bool {
	return strings.HasPrefix(name, "type-") && len(name) == 74
}

// normalizeLegacyDefinition brings a 1.x schema into the strict subset. 1.x
// ignored unknown keywords and unknown types, so dropping them preserves its
// validation behavior exactly.
func normalizeLegacyDefinition(definition Schema, depth int) Schema {
	if definition == nil || depth > 32 {
		return definition
	}
	normalized := make(Schema, len(definition))
	for key, value := range definition {
		switch key {
		case "type":
			if kind, ok := value.(string); ok {
				switch kind {
				case "string", "number", "integer", "boolean", "null", "object", "array":
					normalized[key] = kind
				}
			}
		case "properties":
			if properties, ok := value.(map[string]any); ok {
				children := make(map[string]any, len(properties))
				for name, child := range properties {
					if childDefinition, ok := child.(map[string]any); ok {
						children[name] = map[string]any(normalizeLegacyDefinition(childDefinition, depth+1))
					}
				}
				normalized[key] = children
			}
		case "items":
			if child, ok := value.(map[string]any); ok {
				normalized[key] = map[string]any(normalizeLegacyDefinition(child, depth+1))
			}
		case "required":
			switch typed := value.(type) {
			case bool:
				normalized[key] = typed
			case []any:
				names := make([]any, 0, len(typed))
				for _, name := range typed {
					if _, ok := name.(string); ok {
						names = append(names, name)
					}
				}
				normalized[key] = names
			}
		case "additionalProperties", "allowAdditionalProperties":
			if flag, ok := value.(bool); ok {
				normalized[key] = flag
			}
		case "description", "title":
			normalized[key] = value
		}
	}
	if depth == 0 {
		if _, hasType := normalized["type"]; !hasType {
			if _, hasProperties := normalized["properties"]; !hasProperties {
				normalized["properties"] = map[string]any{}
			}
		}
	}
	return normalized
}

// List fails on unreadable or invalid schemas instead of hiding corruption.
func (registry *Registry) List() ([]RegisteredSchema, error) {
	files, err := os.ReadDir(registry.directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var registered []RegisteredSchema
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		content, err := readDocument(filepath.Join(registry.directory, file.Name()))
		if err != nil {
			return nil, err
		}
		var document schemaDocument
		if isCurrentFileName(file.Name()) {
			if err := json.Unmarshal(content, &document); err != nil {
				return nil, err
			}
			if currentFileName(document.EventType) != file.Name() {
				return nil, fmt.Errorf("schema identity mismatch")
			}
		} else {
			document.EventType = eventTypeFromSchemaFileName(file.Name())
			if err := json.Unmarshal(content, &document.Schema); err != nil {
				return nil, err
			}
			document.Schema = normalizeLegacyDefinition(document.Schema, 0)
		}
		if err := validateDefinition(document.Schema, 0); err != nil {
			return nil, err
		}
		registered = append(registered, RegisteredSchema(document))
	}
	return registered, nil
}

func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalidSchema, message) }
func validateDefinition(definition Schema, depth int) error {
	if definition == nil || depth > 32 {
		return invalid("missing schema or excessive nesting")
	}
	for key := range definition {
		switch key {
		case "type", "properties", "required", "additionalProperties", "allowAdditionalProperties", "items", "description", "title":
		default:
			return invalid("unsupported keyword " + key)
		}
	}
	if value, ok := definition["type"]; ok {
		kind, ok := value.(string)
		if !ok {
			return invalid("type must be a string")
		}
		switch kind {
		case "string", "number", "integer", "boolean", "null", "object", "array":
		default:
			return invalid("unsupported type " + kind)
		}
	}
	for _, key := range []string{"additionalProperties", "allowAdditionalProperties"} {
		if value, ok := definition[key]; ok {
			if _, ok := value.(bool); !ok {
				return invalid(key + " must be boolean")
			}
		}
	}
	if value, ok := definition["properties"]; ok {
		properties, ok := value.(map[string]any)
		if !ok {
			return invalid("properties must be an object")
		}
		for _, value := range properties {
			child, ok := value.(map[string]any)
			if !ok {
				return invalid("property definition must be an object")
			}
			if err := validateDefinition(child, depth+1); err != nil {
				return err
			}
		}
	}
	if value, ok := definition["required"]; ok {
		switch value := value.(type) {
		case bool: // Historical per-property required flag.
		case []any:
			for _, name := range value {
				if _, ok := name.(string); !ok {
					return invalid("required must list field names")
				}
			}
		default:
			return invalid("required must be a boolean or array of field names")
		}
	}
	if value, ok := definition["items"]; ok {
		child, ok := value.(map[string]any)
		if !ok {
			return invalid("items must be a schema object")
		}
		if err := validateDefinition(child, depth+1); err != nil {
			return err
		}
	}
	if depth == 0 {
		if _, hasType := definition["type"]; !hasType {
			if _, hasProperties := definition["properties"]; !hasProperties {
				return invalid("root needs type or properties")
			}
		}
	}
	return nil
}

func (registry *Registry) ValidateEvent(eventType string, data json.RawMessage) error {
	definition, err := registry.Find(eventType)
	if err != nil || definition == nil {
		return err
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	return validateValue(value, definition, "event", 0)
}

func validateValue(value any, definition Schema, path string, depth int) error {
	if depth > 32 {
		return fmt.Errorf("%s exceeds nesting limit", path)
	}
	kind, _ := definition["type"].(string)
	if _, ok := definition["properties"]; ok && kind == "" {
		kind = "object"
	}
	matches := true
	switch kind {
	case "string":
		_, matches = value.(string)
	case "number":
		_, matches = value.(float64)
	case "integer":
		number, ok := value.(float64)
		matches = ok && math.Trunc(number) == number
	case "boolean":
		_, matches = value.(bool)
	case "null":
		matches = value == nil
	case "object":
		_, matches = value.(map[string]any)
	case "array":
		_, matches = value.([]any)
	}
	if !matches {
		if value == nil {
			return fmt.Errorf("%s cannot be null according to schema", path)
		}
		return fmt.Errorf("%s must be a %s", path, kind)
	}
	if object, ok := value.(map[string]any); ok {
		properties, _ := definition["properties"].(map[string]any)
		required := map[string]bool{}
		if names, ok := definition["required"].([]any); ok {
			for _, name := range names {
				required[name.(string)] = true
			}
		}
		for name, property := range properties {
			child := property.(map[string]any)
			if flag, _ := child["required"].(bool); flag {
				required[name] = true
			}
		}
		for name := range required {
			if _, ok := object[name]; !ok {
				return fmt.Errorf("required field '%s' is missing in event data", name)
			}
		}
		additional, _ := definition["allowAdditionalProperties"].(bool)
		if flag, ok := definition["additionalProperties"].(bool); ok {
			additional = flag
		}
		for name, item := range object {
			property, ok := properties[name]
			if !ok {
				if !additional {
					return fmt.Errorf("field '%s' is not allowed by schema", name)
				}
				continue
			}
			child := property.(map[string]any)
			if flag, _ := child["required"].(bool); flag && item == nil {
				return fmt.Errorf("required field '%s' cannot be null", name)
			}
			if err := validateValue(item, child, name, depth+1); err != nil {
				return err
			}
		}
	}
	if items, ok := value.([]any); ok {
		if child, ok := definition["items"].(map[string]any); ok {
			for _, item := range items {
				if err := validateValue(item, child, path+"[]", depth+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
