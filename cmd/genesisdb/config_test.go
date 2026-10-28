package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestConfig_OpenTelemetryFollowsTheMetricsSwitch(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		expected bool
	}{
		{"metrics off", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}, false},
		{"metrics on without endpoint", map[string]string{"GENESISDB_METRICS": "true"}, false},
		{"metrics on with endpoint", map[string]string{"GENESISDB_METRICS": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}, true},
		{"metrics on with metrics endpoint", map[string]string{"GENESISDB_METRICS": "true", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://collector:4318/v1/metrics"}, true},
		{"legacy switch with endpoint", map[string]string{"GENESISDB_PROMETHEUS_METRICS": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}, true},
		{"blank endpoint", map[string]string{"GENESISDB_METRICS": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "  "}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			environment := map[string]string{"GENESISDB_AUTH_TOKEN": strings.Repeat("a", 32)}
			for name, value := range testCase.env {
				environment[name] = value
			}
			lookup := func(name string) (string, bool) {
				value, isSet := environment[name]
				return value, isSet
			}
			config, err := loadConfig(lookup, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if config.IsOpenTelemetryEnabled != testCase.expected {
				t.Errorf("IsOpenTelemetryEnabled = %v, want %v", config.IsOpenTelemetryEnabled, testCase.expected)
			}
		})
	}
}

func TestConfig_CacheSwitchIsCaseInsensitive(t *testing.T) {
	cases := map[string]bool{"": true, "true": true, "TRUE": true, "false": false, "False": false, "FALSE": false, "0": false, "no": false, "garbage": true}
	for value, expected := range cases {
		t.Run("value "+value, func(t *testing.T) {
			environment := map[string]string{"GENESISDB_AUTH_TOKEN": strings.Repeat("a", 32)}
			if value != "" {
				environment["GENESISDB_MEMCACHE"] = value
			}
			lookup := func(name string) (string, bool) {
				text, isSet := environment[name]
				return text, isSet
			}
			config, err := loadConfig(lookup, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if config.IsCacheEnabled != expected {
				t.Errorf("IsCacheEnabled = %v, want %v", config.IsCacheEnabled, expected)
			}
			if expected && config.CacheSize != 0 || !expected && config.CacheSize != -1 {
				t.Errorf("CacheSize = %d", config.CacheSize)
			}
		})
	}
}

// Like 1.x the server starts with any arguments, unknown ones are reported.
func TestUnknownArgumentsAreReportedNotFatal(t *testing.T) {
	for _, arguments := range [][]string{nil, {"serve"}, {"start"}} {
		if warning := unknownArgumentsWarning(arguments); warning != "" {
			t.Errorf("%q: unexpected warning %q", arguments, warning)
		}
	}
	for _, arguments := range [][]string{{"bogus"}, {"--port", "9000"}, {"serve", "extra"}} {
		if warning := unknownArgumentsWarning(arguments); warning == "" {
			t.Errorf("%q: no warning", arguments)
		}
	}
}
