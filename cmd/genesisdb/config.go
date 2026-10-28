package main

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/store"
)

// Config is the complete runtime configuration. It is read once from the
// environment (and an optional .env file) and passed down. No package reads
// environment variables on its own.
type Config struct {
	Port                  string
	Host                  string
	TLSCertFile           string
	TLSKeyFile            string
	ReadToken             string
	WriteToken            string
	MaxRequestBytes       int64
	MaxRestoreBytes       int64
	MaxReadEvents         int
	MaxReadBytes          int64
	MaxConcurrentRequests int
	RequestTimeout        time.Duration
	DataDirectory         string
	AuthToken             string
	TimeZone              string
	IsCacheEnabled        bool
	CacheSize             int
	IsMetricsEnabled      bool
	MetricsPort           string
	// IsOpenTelemetryEnabled pushes metrics with OTLP over HTTP. It is on
	// when metrics are enabled and an OTLP endpoint is configured. Headers
	// and interval come from the standard OTEL_* variables, which the
	// OpenTelemetry SDK reads itself.
	IsOpenTelemetryEnabled bool
	FormatPolicy           store.FormatPolicy
	IncludeLegacyNames     bool
	LogLevel               slog.Level
	// IsMCPEnabled serves the Model Context Protocol endpoint at /mcp.
	IsMCPEnabled bool
}

// Environment variables that earlier releases understood and that are gone.
var removedVariables = []string{"GENESISDB_GRPC_ENABLED", "GENESISDB_GRPC_PORT", "GENESISDB_LICENSE"}

// loadConfig reads the configuration from lookup, normally os.LookupEnv.
func loadConfig(lookup func(string) (string, bool), logger *slog.Logger) (*Config, error) {
	value := func(name, fallback string) string {
		if text, isSet := lookup(name); isSet && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
		return fallback
	}
	isTrue := func(name string, fallback bool) bool {
		text := strings.ToLower(value(name, strconv.FormatBool(fallback)))
		return text == "true" || text == "1" || text == "yes"
	}
	// isFalse is for settings that are on unless explicitly switched off, so
	// an unknown value keeps the default as every earlier release did.
	isFalse := func(name string) bool {
		text := strings.ToLower(value(name, "true"))
		return text == "false" || text == "0" || text == "no"
	}

	config := &Config{
		Port:               value("GENESISDB_PORT", "8080"),
		Host:               value("GENESISDB_HOST", "127.0.0.1"),
		TLSCertFile:        value("GENESISDB_TLS_CERT_FILE", ""),
		TLSKeyFile:         value("GENESISDB_TLS_KEY_FILE", ""),
		ReadToken:          value("GENESISDB_READ_TOKEN", ""),
		WriteToken:         value("GENESISDB_WRITE_TOKEN", ""),
		AuthToken:          value("GENESISDB_AUTH_TOKEN", ""),
		TimeZone:           value("GENESISDB_TZ", ""),
		IsCacheEnabled:     !isFalse("GENESISDB_MEMCACHE"),
		IsMetricsEnabled:   isTrue("GENESISDB_METRICS", false) || isTrue("GENESISDB_PROMETHEUS_METRICS", false),
		MetricsPort:        value("GENESISDB_METRICS_PORT", "2112"),
		IncludeLegacyNames: isTrue("GENESISDB_LEGACY_METRICS", true),
		IsMCPEnabled:       isTrue("GENESISDB_MCP", false),
		LogLevel:           slog.LevelInfo,
	}
	// Without an endpoint the SDK would push to localhost:4318 and log an
	// error on every interval, so OpenTelemetry needs an explicit target.
	config.IsOpenTelemetryEnabled = config.IsMetricsEnabled && (value("OTEL_EXPORTER_OTLP_ENDPOINT", "") != "" || value("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "") != "")
	if config.AuthToken == "" {
		return nil, errors.New("required environment variable GENESISDB_AUTH_TOKEN is not set")
	}
	if (config.TLSCertFile == "") != (config.TLSKeyFile == "") {
		return nil, errors.New("both TLS certificate and key files are required")
	}
	seenTokens := map[string]bool{}
	for _, token := range []string{config.AuthToken, config.ReadToken, config.WriteToken} {
		if token == "" {
			continue
		}
		if len(token) < 32 {
			return nil, errors.New("authentication tokens must contain at least 32 bytes, generate them with openssl rand -hex 32")
		}
		if seenTokens[token] {
			return nil, errors.New("authentication tokens for different roles must be distinct")
		}
		seenTokens[token] = true
	}
	// Every limit is opt-in. Unset or 0 means unbounded, which is the
	// behavior of every earlier release: nothing is truncated or refused
	// because a store, a batch or a backup grew.
	optionalLimit := func(name string) (int64, error) {
		n, err := strconv.ParseInt(value(name, "0"), 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%s must be 0 (unbounded) or a positive integer", name)
		}
		return n, nil
	}
	var err error
	for _, setting := range []struct {
		name   string
		target *int64
	}{
		{"GENESISDB_MAX_REQUEST_BYTES", &config.MaxRequestBytes},
		{"GENESISDB_MAX_RESTORE_BYTES", &config.MaxRestoreBytes},
		{"GENESISDB_MAX_READ_BYTES", &config.MaxReadBytes},
	} {
		*setting.target, err = optionalLimit(setting.name)
		if err != nil {
			return nil, err
		}
	}
	n, err := optionalLimit("GENESISDB_MAX_READ_EVENTS")
	if err != nil {
		return nil, err
	}
	config.MaxReadEvents = int(n)
	n, err = optionalLimit("GENESISDB_MAX_CONCURRENT_REQUESTS")
	if err != nil {
		return nil, err
	}
	config.MaxConcurrentRequests = int(n)
	n, err = optionalLimit("GENESISDB_REQUEST_TIMEOUT_SECONDS")
	if err != nil {
		return nil, err
	}
	config.RequestTimeout = time.Duration(n) * time.Second
	directory, err := filepath.Abs(value("GENESISDB_DATA_DIR", "data"))
	if err != nil {
		return nil, fmt.Errorf("GENESISDB_DATA_DIR: %w", err)
	}
	config.DataDirectory = directory

	config.CacheSize = 0
	if text := value("GENESISDB_MEMCACHE_SIZE", ""); text != "" {
		size, err := strconv.Atoi(text)
		if err != nil || size <= 0 {
			return nil, fmt.Errorf("GENESISDB_MEMCACHE_SIZE must be a positive number, got %q", text)
		}
		config.CacheSize = size
	}
	if !config.IsCacheEnabled {
		config.CacheSize = -1
	}
	config.FormatPolicy, err = store.ParseFormatPolicy(value("GENESISDB_STORAGE_FORMAT", "auto"))
	if err != nil {
		return nil, fmt.Errorf("GENESISDB_STORAGE_FORMAT: %w", err)
	}
	if level := value("GENESISDB_LOG_LEVEL", ""); level != "" {
		if err := config.LogLevel.UnmarshalText([]byte(level)); err != nil {
			return nil, fmt.Errorf("GENESISDB_LOG_LEVEL: %w", err)
		}
	}
	for _, name := range removedVariables {
		if _, isSet := lookup(name); isSet {
			logger.Warn("environment variable is no longer supported and is ignored", "variable", name)
		}
	}
	return config, nil
}

// applyTimeZone sets the time zone of event timestamps and log lines.
//
// time.Local is replaced directly. Setting the TZ environment variable
// instead only works before the first time formatting, which the startup
// log lines already did at this point.
func applyTimeZone(config *Config, logger *slog.Logger) {
	if config.TimeZone == "" {
		return
	}
	location, err := time.LoadLocation(config.TimeZone)
	if err != nil {
		logger.Warn("failed to set time zone, using the system time zone", "timeZone", config.TimeZone, "error", err)
		return
	}
	time.Local = location
}
