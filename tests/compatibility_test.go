package tests

// The compatibility suite is the release gate for 1.1.6 compatibility. Its
// fixtures under testdata/compatibility were produced by GenesisDB 1.1.6:
//
//   - v116: a data directory as 1.1.6 leaves it (fixed-shard indexes,
//     reference data, an erasure with its system event, a schema)
//   - v115-legacy-indexes: the same events with the per-key index layout of
//     releases before 1.1.6
//   - golden.json: requests against v116 and the exact responses of 1.1.6
//   - backup-*-v116.json: backups exported by 1.1.6.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
	"github.com/genesisdb-io/genesisdb/v2/packages/query"
	"github.com/genesisdb-io/genesisdb/v2/packages/server/httpserver"
	"github.com/genesisdb-io/genesisdb/v2/packages/store"
)

const fixtureDirectory = "testdata/compatibility"

type goldenExchange struct {
	Name        string            `json:"name"`
	Route       string            `json:"route"`
	Method      string            `json:"method"`
	Body        string            `json:"body"`
	Headers     map[string]string `json:"headers"`
	Status      int               `json:"status"`
	ContentType string            `json:"contentType"`
	Response    string            `json:"response"`
}

// copyFixture copies a fixture data directory into a temporary directory.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), name)
	if output, err := exec.Command("cp", "-R", filepath.Join(fixtureDirectory, name), target).CombinedOutput(); err != nil {
		t.Fatalf("copy fixture: %v %s", err, output)
	}
	return target
}

func openTestStore(t *testing.T, directory string, policy store.FormatPolicy) *store.Store {
	t.Helper()
	opened, err := store.Open(directory, store.Options{CacheSize: -1, FormatPolicy: policy, EvaluateGDBQLPrecondition: query.EvaluatePrecondition, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return opened
}

func loadGoldenExchanges(t *testing.T) []goldenExchange {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(fixtureDirectory, "golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exchanges []goldenExchange
	if err := json.Unmarshal(content, &exchanges); err != nil {
		t.Fatal(err)
	}
	return exchanges
}

// normalizeResponse makes responses comparable where 1.1.6 was not
// deterministic: Go map iteration order in GROUP BY results.
func normalizeResponse(name, response string) string {
	if name == "backup" {
		backup, err := core.DecodeBackup([]byte(response))
		if err == nil {
			backup.Schemas = nil
			var buffer bytes.Buffer
			if backup.EncodeJSON(&buffer) == nil {
				return buffer.String()
			}
		}
	}
	if !strings.HasPrefix(name, "query-b1") {
		return response
	}
	lines := strings.Split(strings.TrimSpace(response), "\n\n")
	sortStrings(lines)
	return strings.Join(lines, "\n\n")
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// replayGoldenExchanges sends every recorded request to handler and compares
// status, content type and body byte for byte.
func replayGoldenExchanges(t *testing.T, handler http.Handler, route func(goldenExchange) string) {
	t.Helper()
	for _, exchange := range loadGoldenExchanges(t) {
		// The old IN parser failed to trim whitespace before unquoting list
		// elements. Both listed types must match after the correctness fix.
		if exchange.Name == "query-a6" {
			exchange.Response = "{\"t\":\"io.genesisdb.app.address-changed\"}\n\n{\"t\":\"io.genesisdb.store.article-added\"}\n\n"
		}
		request := httptest.NewRequest(exchange.Method, route(exchange), strings.NewReader(exchange.Body))
		request.Header.Set("Authorization", "Bearer secret")
		for name, value := range exchange.Headers {
			request.Header.Set(name, value)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		body, _ := io.ReadAll(recorder.Body)
		if recorder.Code != exchange.Status {
			t.Errorf("%s: status %d, 1.1.6 answered %d (%s)", exchange.Name, recorder.Code, exchange.Status, body)
			continue
		}
		if contentType := recorder.Header().Get("Content-Type"); contentType != exchange.ContentType {
			t.Errorf("%s: content type %q, 1.1.6 answered %q", exchange.Name, contentType, exchange.ContentType)
		}
		got, want := normalizeResponse(exchange.Name, string(body)), normalizeResponse(exchange.Name, exchange.Response)
		if got != want {
			t.Errorf("%s: response differs from 1.1.6\n got: %.600s\nwant: %.600s", exchange.Name, got, want)
		}
	}
}

// version1Route replays a recorded exchange on the route 1.1.6 answered.
func version1Route(exchange goldenExchange) string { return exchange.Route }

// version2Route maps a recorded 1.1.6 route to its /api/v2 successor, so the
// current API is verified against the same golden responses.
func version2Route(exchange goldenExchange) string {
	replacements := map[string]string{
		"/api/v1/status":         "/api/v2/status",
		"/api/v1/status/ping":    "/api/v2/ping",
		"/api/v1/status/audit":   "/api/v2/admin/audit",
		"/api/v1/commit":         "/api/v2/commit",
		"/api/v1/stream":         "/api/v2/fetch",
		"/api/v1/q":              "/api/v2/query",
		"/api/v1/query":          "/api/v2/query",
		"/api/v1/subjects":       "/api/v2/subjects",
		"/api/v1/types":          "/api/v2/types",
		"/api/v1/backup/create":  "/api/v2/admin/backup",
		"/api/v1/backup/restore": "/api/v2/admin/restore",
		"/api/v1/schema/get":     "/api/v2/schemas",
		"/api/v1/erase":          "/api/v2/admin/erase",
	}
	successor, exists := replacements[exchange.Route]
	if !exists {
		panic("no /api/v2 successor for recorded route " + exchange.Route)
	}
	return successor
}

func newTestServer(s *store.Store) http.Handler {
	return httpserver.New(s, httpserver.Options{AuthToken: "secret", Logger: quietLogger()}).Routes()
}

func TestCompatibility_LegacyDirectoryAnswersLikeVersion116(t *testing.T) {
	directory := copyFixture(t, "v116")
	s := openTestStore(t, directory, store.KeepExistingFormat)
	defer s.Close()
	if s.StorageFormat() != layout.FormatLegacy {
		t.Fatalf("format %s, want legacy", s.StorageFormat())
	}
	t.Run("api v1", func(t *testing.T) { replayGoldenExchanges(t, newTestServer(s), version1Route) })
	t.Run("api v2", func(t *testing.T) { replayGoldenExchanges(t, newTestServer(s), version2Route) })
}

func TestCompatibility_ConvertedDirectoryAnswersLikeVersion116(t *testing.T) {
	directory := copyFixture(t, "v116")
	s := openTestStore(t, directory, store.ConvertToCurrentFormat)
	defer s.Close()
	if s.StorageFormat() != layout.FormatCurrent {
		t.Fatalf("format %s, want current", s.StorageFormat())
	}
	assertNoLegacyFiles(t, directory)
	replayGoldenExchanges(t, newTestServer(s), version1Route)
}

func TestCompatibility_PreVersion116IndexesAreMigrated(t *testing.T) {
	directory := copyFixture(t, "v115-legacy-indexes")
	s := openTestStore(t, directory, store.KeepExistingFormat)
	defer s.Close()
	replayGoldenExchanges(t, newTestServer(s), version1Route)
}

func TestCompatibility_HashChainOfVersion116IsValid(t *testing.T) {
	for _, policy := range []store.FormatPolicy{store.KeepExistingFormat, store.ConvertToCurrentFormat} {
		s := openTestStore(t, copyFixture(t, "v116"), policy)
		report, err := s.VerifyHashChain(context.Background())
		s.Close()
		if err != nil || !report.IsValid() || report.CheckedEvents != 11 {
			t.Fatalf("policy %d: %+v %v", policy, report, err)
		}
	}
}

// The authoritative logs must stay byte identical when a legacy directory
// is opened and closed without writes.
func TestCompatibility_LegacyLogsAreNotModified(t *testing.T) {
	directory := copyFixture(t, "v116")
	before := readLogs(t, directory)
	s := openTestStore(t, directory, store.KeepExistingFormat)
	s.Close()
	after := readLogs(t, directory)
	for name, content := range before {
		if !bytes.Equal(after[name], content) {
			t.Errorf("%s changed", name)
		}
	}
	if _, err := os.Stat(filepath.Join(directory, layout.FormatMarkerFile)); err == nil {
		t.Error("a legacy directory must not get a format marker in auto mode")
	}
}

func readLogs(t *testing.T, directory string) map[string][]byte {
	t.Helper()
	logs := map[string][]byte{}
	entries, _ := os.ReadDir(filepath.Join(directory, layout.EventsDirectory))
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(directory, layout.EventsDirectory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		logs[entry.Name()] = content
	}
	return logs
}

func assertNoLegacyFiles(t *testing.T, directory string) {
	t.Helper()
	filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".eventstore") {
			t.Errorf("legacy file left after conversion: %s", path)
		}
		if !info.IsDir() && strings.HasSuffix(path, ".gdb") && strings.Contains(path, string(filepath.Separator)+layout.EventsDirectory+string(filepath.Separator)) {
			first, _ := os.ReadFile(path)
			if !bytes.HasPrefix(first, []byte("genesisdb\n")) {
				t.Errorf("%s does not start with the genesisdb header", path)
			}
		}
		return nil
	})
}

func restoreBackupFile(t *testing.T, handler http.Handler, route, file string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(fixtureDirectory, file))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, route, bytes.NewReader(content))
	request.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("restore %s: %d %s", file, recorder.Code, recorder.Body)
	}
	return recorder.Body.String()
}

func TestCompatibility_BackupsOfVersion116Restore(t *testing.T) {
	for _, file := range []string{"backup-extended-v116.json", "backup-plain-v116.json"} {
		for _, route := range []string{"/api/v1/backup/restore", "/api/v2/admin/restore"} {
			s := openTestStore(t, t.TempDir(), store.KeepExistingFormat)
			restoreBackupFile(t, newTestServer(s), route, file)
			report, err := s.VerifyHashChain(context.Background())
			if err != nil || !report.IsValid() {
				t.Fatalf("%s via %s: %+v %v", file, route, report, err)
			}
			s.Close()
		}
	}
}

// Historical backup sections remain byte compatible. The new schemas section
// is additive and is checked separately rather than discarded by the test.
func TestCompatibility_BackupPreservesVersion116Sections(t *testing.T) {
	s := openTestStore(t, copyFixture(t, "v116"), store.KeepExistingFormat)
	defer s.Close()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/backup/create", nil)
	request.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	newTestServer(s).ServeHTTP(recorder, request)
	want, _ := os.ReadFile(filepath.Join(fixtureDirectory, "backup-extended-v116.json"))
	if normalizeResponse("backup", recorder.Body.String()) != normalizeResponse("backup", string(want)) {
		t.Fatal("historical backup sections differ from the 1.1.6 export")
	}
	backup, err := core.DecodeBackup(recorder.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// Serving the directory migrated the 1.1.6 schema file into the current
	// envelope. The backup carries that envelope with the exact type.
	legacy, err := os.ReadFile(filepath.Join(fixtureDirectory, "v116", "schemas", "io_genesisdb_app_customer-added.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wantSchema any
	json.Unmarshal(legacy, &wantSchema)
	wantJSON, _ := json.Marshal(wantSchema)
	found := false
	for name, content := range backup.Schemas {
		var document struct {
			EventType string `json:"eventType"`
			Schema    any    `json:"schema"`
		}
		if err := json.Unmarshal(content, &document); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if document.EventType != "io.genesisdb.app.customer-added" {
			continue
		}
		found = true
		if gotJSON, _ := json.Marshal(document.Schema); string(gotJSON) != string(wantJSON) {
			t.Fatalf("migrated schema differs: %s, want %s", gotJSON, wantJSON)
		}
	}
	if !found {
		t.Fatalf("migrated schema missing from backup: %v", backup.Schemas)
	}
}

// The default configuration is a drop-in replacement for a 1.1.6 data
// directory: a pending 1.1.6 WAL is replayed, logs and indexes are migrated,
// the schema file is migrated, and every request answers like 1.1.6 did.
func TestCompatibility_DefaultOpenMigratesVersion116DirectoryCompletely(t *testing.T) {
	directory := copyFixture(t, "v116")
	legacyWAL := filepath.Join(directory, layout.WALDirectory, "pending.wal")
	os.MkdirAll(filepath.Dir(legacyWAL), 0o755)
	// A 1.1.6 WAL: one {"event":...} line per entry, written before the
	// event log append. The events are committed by 1.1.6 right before it
	// was stopped, so their hashes chain onto the fixture's last event.
	pending := &core.Event{Source: "io.genesisdb.app", Subject: "/customer/6", Type: "io.genesisdb.app.customer-added", SpecVersion: "1.0", ID: "11111111-2222-4333-8444-555555555555", Time: time.Date(2026, 10, 23, 19, 3, 0, 0, time.UTC), DataContentType: "application/json", Data: json.RawMessage(`{"firstName":"Harvey","age":45}`), PredecessorHash: "5404b4104bdc67f226499d995bb58552a1ab5681ef42b9115716acf4efb7101b"}
	pending.Hash = core.HashEvent(pending)
	line, _ := json.Marshal(map[string]any{"event": pending})
	if err := os.WriteFile(legacyWAL, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	opened, err := store.Open(directory, store.Options{EvaluateGDBQLPrecondition: query.EvaluatePrecondition, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("default open of a 1.1.6 directory failed: %v", err)
	}
	defer opened.Close()
	if opened.StorageFormat() != layout.FormatCurrent {
		t.Fatalf("format %s, want current", opened.StorageFormat())
	}
	assertNoLegacyFiles(t, directory)
	if count, _ := opened.CountEvents(); count != 12 {
		t.Fatalf("%d events, want 11 fixture events plus 1 replayed from the 1.1.6 WAL", count)
	}
	report, err := opened.VerifyHashChain(context.Background())
	if err != nil || !report.IsValid() {
		t.Fatalf("hash chain after migration: %+v %v", report, err)
	}
	if info, err := os.Stat(legacyWAL); err == nil && info.Size() != 0 {
		t.Fatal("replayed 1.1.6 WAL not cleared")
	}

	handler := newTestServer(opened)
	if _, err := os.Stat(filepath.Join(directory, layout.SchemasDirectory, "io_genesisdb_app_customer-added.json")); !os.IsNotExist(err) {
		t.Fatal("legacy schema file not migrated")
	}
	// The migrated schema still enforces what 1.1.6 enforced.
	request := httptest.NewRequest(http.MethodPost, "/api/v1/commit", strings.NewReader(`{"events":[{"source":"a","subject":"/customer/7","type":"io.genesisdb.app.customer-added","data":{"age":1}}]}`))
	request.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "firstName") {
		t.Fatalf("migrated schema not enforced: %d %s", recorder.Code, recorder.Body.String())
	}
	if _, err := opened.FindEventByID(context.Background(), pending.ID); err != nil {
		t.Fatalf("event replayed from the 1.1.6 WAL is not indexed: %v", err)
	}
}

// With default options a 1.1.6 directory answers every recorded request
// exactly like 1.1.6, on /api/v1 and on the /api/v2 successors.
func TestCompatibility_DefaultOpenAnswersLikeVersion116(t *testing.T) {
	directory := copyFixture(t, "v116")
	s, err := store.Open(directory, store.Options{EvaluateGDBQLPrecondition: query.EvaluatePrecondition, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	handler := newTestServer(s)
	t.Run("api v1", func(t *testing.T) { replayGoldenExchanges(t, handler, version1Route) })
	t.Run("api v2", func(t *testing.T) { replayGoldenExchanges(t, handler, version2Route) })
}

// Every persisted structure must encode exactly as in 1.1.6.
func TestCompatibility_PersistedRecordsEncodeLikeVersion116(t *testing.T) {
	for _, directory := range []string{"v116"} {
		for _, logName := range []string{"events.eventstore", "refs.eventstore", "system.eventstore"} {
			content, err := os.ReadFile(filepath.Join(fixtureDirectory, directory, "events", logName))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
			for _, line := range lines[1:] {
				var reencoded []byte
				if logName == "refs.eventstore" {
					var entry core.ReferenceDataEntry
					if err := json.Unmarshal([]byte(line), &entry); err != nil {
						t.Fatal(err)
					}
					reencoded, _ = json.Marshal(entry)
				} else {
					var record struct {
						Event *core.Event `json:"event"`
					}
					if err := json.Unmarshal([]byte(line), &record); err != nil {
						t.Fatal(err)
					}
					reencoded, _ = json.Marshal(record)
				}
				if string(reencoded) != line {
					t.Errorf("%s: record encodes differently\n got: %s\nwant: %s", logName, reencoded, line)
				}
			}
		}
	}
}
