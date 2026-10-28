package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSecureConfigurationDefaultsAndValidation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	load := func(extra map[string]string) (*Config, error) {
		env := map[string]string{"GENESISDB_AUTH_TOKEN": strings.Repeat("a", 32)}
		for key, value := range extra {
			env[key] = value
		}
		return loadConfig(func(key string) (string, bool) {
			value, ok := env[key]
			return value, ok
		}, logger)
	}
	config, err := load(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Every limit is opt-in: unset means unbounded.
	if config.Host != "127.0.0.1" || config.MaxConcurrentRequests != 0 || config.MaxRequestBytes != 0 || config.MaxRestoreBytes != 0 || config.MaxReadEvents != 0 || config.MaxReadBytes != 0 || config.RequestTimeout != 0 {
		t.Fatalf("unexpected security defaults: %+v", config)
	}
	limited, err := load(map[string]string{"GENESISDB_MAX_READ_EVENTS": "500", "GENESISDB_MAX_CONCURRENT_REQUESTS": "8", "GENESISDB_REQUEST_TIMEOUT_SECONDS": "30"})
	if err != nil {
		t.Fatal(err)
	}
	if limited.MaxReadEvents != 500 || limited.MaxConcurrentRequests != 8 || limited.RequestTimeout != 30*time.Second {
		t.Fatalf("explicit limits not applied: %+v", limited)
	}
	for name, env := range map[string]map[string]string{
		"short admin token":       {"GENESISDB_AUTH_TOKEN": "short"},
		"short reader token":      {"GENESISDB_READ_TOKEN": "short"},
		"equal role tokens":       {"GENESISDB_READ_TOKEN": strings.Repeat("a", 32)},
		"missing key":             {"GENESISDB_TLS_CERT_FILE": "certificate.pem"},
		"missing certificate":     {"GENESISDB_TLS_KEY_FILE": "key.pem"},
		"invalid body budget":     {"GENESISDB_MAX_REQUEST_BYTES": "many"},
		"negative restore budget": {"GENESISDB_MAX_RESTORE_BYTES": "-1"},
		"negative read budget":    {"GENESISDB_MAX_READ_EVENTS": "-5"},
		"invalid concurrency":     {"GENESISDB_MAX_CONCURRENT_REQUESTS": "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := load(env); err == nil {
				t.Fatal("invalid security configuration accepted")
			}
		})
	}
	if _, err := load(map[string]string{
		"GENESISDB_READ_TOKEN":    strings.Repeat("b", 32),
		"GENESISDB_WRITE_TOKEN":   strings.Repeat("c", 32),
		"GENESISDB_TLS_CERT_FILE": "certificate.pem",
		"GENESISDB_TLS_KEY_FILE":  "key.pem",
	}); err != nil {
		t.Fatal(err)
	}
}
