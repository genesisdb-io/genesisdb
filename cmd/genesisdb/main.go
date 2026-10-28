// Command genesisdb runs the GenesisDB server.
//
//	genesisdb                  start the server (configured by environment)
//	genesisdb convert [flags]  migrate a stopped 1.x data directory offline (the server does this automatically on start)
//	genesisdb version          print the version
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/joho/godotenv"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
	"github.com/genesisdb-io/genesisdb/v2/packages/metrics"
	"github.com/genesisdb-io/genesisdb/v2/packages/query"
	"github.com/genesisdb-io/genesisdb/v2/packages/server/httpserver"
	"github.com/genesisdb-io/genesisdb/v2/packages/store"
	"github.com/genesisdb-io/genesisdb/v2/packages/telemetry"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "convert":
			os.Exit(runConvert(os.Args[2:], logger))
		case "version", "--version", "-v":
			fmt.Println("GenesisDB", core.Version)
			return
		case "help", "--help", "-h":
			printUsage()
			return
		default:
			if warning := unknownArgumentsWarning(os.Args[1:]); warning != "" {
				logger.Warn(warning)
			}
		}
	}
	if err := runServer(logger); err != nil {
		logger.Error("GenesisDB stopped with an error", "error", err)
		os.Exit(1)
	}
}

// unknownArgumentsWarning explains ignored arguments. Like 1.x, the server
// starts with any arguments, so existing start scripts (for example
// "genesisdb serve") keep working. serve and start are accepted silently.
func unknownArgumentsWarning(arguments []string) string {
	if len(arguments) == 0 || (len(arguments) == 1 && (arguments[0] == "serve" || arguments[0] == "start")) {
		return ""
	}
	return fmt.Sprintf("ignoring unknown arguments %q, the server is configured by GENESISDB_* environment variables, see genesisdb help", arguments)
}

func printUsage() {
	fmt.Print(`Usage:
  genesisdb                  start the server, configured by GENESISDB_* environment variables
  genesisdb convert [flags]  migrate a stopped 1.x data directory offline (the server does this automatically on start)
  genesisdb version          print the version
`)
}

func runServer(logger *slog.Logger) error {
	if err := godotenv.Load(); err == nil {
		logger.Info("loaded environment from .env")
	}
	config, err := loadConfig(os.LookupEnv, logger)
	if err != nil {
		return err
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: config.LogLevel}))
	slog.SetDefault(logger)
	applyTimeZone(config, logger)

	var observers core.MultiObserver
	var collector *metrics.Collector
	if config.IsMetricsEnabled {
		collector = metrics.New(metrics.Options{IncludeLegacyNames: config.IncludeLegacyNames})
		observers = append(observers, collector)
	}
	if config.IsOpenTelemetryEnabled {
		exporter, err := telemetry.New(context.Background(), telemetry.Options{ServiceVersion: core.Version})
		if err != nil {
			return err
		}
		// Runs after the store is closed, so the final counts are pushed.
		defer func() {
			shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := exporter.Shutdown(shutdownContext); err != nil {
				logger.Warn("failed to push the final OpenTelemetry metrics", "error", err)
			}
		}()
		observers = append(observers, exporter)
		logger.Info("OpenTelemetry metrics enabled", "protocol", "otlp/http")
	}
	var observer core.Observer = core.NoopObserver{}
	if len(observers) > 0 {
		observer = observers
	}

	// Ownership, format detection, WAL recovery and index validation all
	// finish before any listener opens. Failures are fatal.
	eventStore, err := store.Open(config.DataDirectory, store.Options{
		CacheSize:                 config.CacheSize,
		FormatPolicy:              config.FormatPolicy,
		EvaluateGDBQLPrecondition: query.EvaluatePrecondition,
		Observer:                  observer,
		Logger:                    logger,
	})
	if err != nil {
		return fmt.Errorf("failed to open the data directory: %w", err)
	}
	logger.Info("Successfully initialized event store with directory: " + config.DataDirectory)
	if config.IsCacheEnabled {
		logger.Info("Hydrated event cache is enabled (bounded LRU, GENESISDB_MEMCACHE_SIZE)")
	} else {
		logger.Info("Hydrated event cache is disabled")
	}

	var servers []*http.Server
	serveError := make(chan error, 2)
	if collector != nil {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("GET /metrics", collector.Handler())
		metricsServer := &http.Server{Addr: net.JoinHostPort(config.Host, config.MetricsPort), Handler: metricsMux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
		servers = append(servers, metricsServer)
		go func() {
			if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveError <- fmt.Errorf("metrics server: %w", err)
			}
		}()
		logger.Info("Starting metrics server on :" + config.MetricsPort)
		logger.Info("Registered route: GET /metrics")
	} else {
		logger.Info("Metrics are disabled")
	}

	api := httpserver.New(eventStore, httpserver.Options{
		AuthToken: config.AuthToken, ReadToken: config.ReadToken, WriteToken: config.WriteToken, Logger: logger,
		MaxRequestBytes: config.MaxRequestBytes, MaxRestoreBytes: config.MaxRestoreBytes,
		MaxReadEvents: config.MaxReadEvents, MaxReadBytes: config.MaxReadBytes,
		MaxConcurrentRequests: config.MaxConcurrentRequests, RequestTimeout: config.RequestTimeout,
		EnableMCP: config.IsMCPEnabled,
	})
	// Only the header read is bounded. Bodies (large commits, restores) and
	// responses (large fetches, backups) run to completion.
	// TLS 1.2 is the standard library default, it is pinned here so a GODEBUG
	// setting in the environment cannot lower it.
	apiServer := &http.Server{Addr: net.JoinHostPort(config.Host, config.Port), Handler: api.Routes(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	servers = append(servers, apiServer)
	go func() {
		if config.TLSCertFile != "" {
			serveError <- apiServer.ListenAndServeTLS(config.TLSCertFile, config.TLSKeyFile)
			return
		}
		serveError <- apiServer.ListenAndServe()
	}()
	logger.Info("Environment configuration:")
	logger.Info("- Store Directory: " + config.DataDirectory)
	logger.Info("- Storage Format: " + eventStore.StorageFormat().String())
	logger.Info("- HTTP Port: " + config.Port)
	if config.TLSCertFile != "" {
		logger.Info("- TLS: enabled")
	}
	if config.IsMCPEnabled {
		logger.Info("- MCP endpoint: /mcp")
	}
	logger.Info("HTTP server starting on port " + config.Port)
	logger.Info("GenesisDB is up and running", "version", core.Version)
	printWelcome()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	var listenerError error
	select {
	case received := <-signals:
		logger.Info("shutting down", "signal", received.String())
	case err := <-serveError:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenerError = err
			logger.Error("HTTP server failed", "error", err)
		}
	}
	// Subscriptions are long-lived, so the graceful wait is bounded.
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdownContext); err != nil {
			server.Close()
		}
	}
	if err := eventStore.Close(); err != nil {
		return fmt.Errorf("failed to close the data directory: %w", err)
	}
	logger.Info("GenesisDB stopped")
	return listenerError
}

// printWelcome prints the welcome box of every earlier release to stdout.
// It is plain text next to the log lines, so a container log shows it too.
func printWelcome() {
	lines := []string{
		"Welcome to GenesisDB " + core.Version + "!",
		"",
		"GenesisDB is open source and free to use, without limits.",
		"Every feature is included: GDBQL, schemas, preconditions,",
		"hash chain audit, backups and reference payload erasure.",
		"",
		"Documentation and source: https://github.com/genesisdb-io/genesisdb",
		"Visit https://genesisdb.io for more information.",
	}
	width := 0
	for _, line := range lines {
		if n := utf8.RuneCountInString(line); n > width {
			width = n
		}
	}
	var b strings.Builder
	b.WriteString("\n╔" + strings.Repeat("═", width+2) + "╗\n")
	for _, line := range lines {
		b.WriteString("║ " + line + strings.Repeat(" ", width-utf8.RuneCountInString(line)) + " ║\n")
	}
	b.WriteString("╚" + strings.Repeat("═", width+2) + "╝\n\n")
	fmt.Print(b.String())
}

func runConvert(arguments []string, logger *slog.Logger) int {
	flags := flag.NewFlagSet("convert", flag.ContinueOnError)
	dataDirectory := flags.String("data-dir", os.Getenv("GENESISDB_DATA_DIR"), "data directory to convert (default: GENESISDB_DATA_DIR or ./data)")
	isDryRun := flags.Bool("dry-run", false, "list the files that would be converted without changing anything")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *dataDirectory == "" {
		*dataDirectory = "data"
	}
	report, err := store.Convert(*dataDirectory, layout.FormatCurrent, *isDryRun, store.Options{EvaluateGDBQLPrecondition: query.EvaluatePrecondition, Logger: logger})
	if err != nil {
		logger.Error("conversion failed; run the command again to resume", "error", err)
		return 1
	}
	switch {
	case report.From == report.To:
		fmt.Printf("%s already uses the %s storage format, nothing to do\n", *dataDirectory, report.To)
	case report.IsDryRun:
		fmt.Printf("Would convert %d files from the %s to the %s storage format:\n", len(report.ConvertedFiles), report.From, report.To)
		for _, name := range report.ConvertedFiles {
			fmt.Println("  " + name)
		}
	default:
		fmt.Printf("Converted %d files from the %s to the %s storage format and rebuilt the indexes\n", len(report.ConvertedFiles), report.From, report.To)
	}
	return 0
}
