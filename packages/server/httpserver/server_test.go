package httpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/query"
	"github.com/genesisdb-io/genesisdb/v2/packages/store"
)

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := store.Open(t.TempDir(), store.Options{EvaluateGDBQLPrecondition: query.EvaluatePrecondition, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(s, Options{AuthToken: "secret", Logger: logger}).Routes())
	t.Cleanup(func() {
		server.Close()
		s.Close()
	})
	return server, s
}

func send(t *testing.T, server *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return string(body)
}

func TestAuthentication(t *testing.T) {
	server, _ := newTestServer(t)
	response, _ := http.Post(server.URL+"/api/v2/fetch", "application/json", strings.NewReader(`{"subject":"/"}`))
	if response.StatusCode != http.StatusUnauthorized || strings.TrimSpace(readBody(t, response)) != "No token provided" {
		t.Fatal("request without token accepted")
	}
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v2/fetch", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer wrong")
	response, _ = http.DefaultClient.Do(request)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("wrong token accepted")
	}
	for _, path := range []string{"/ping", "/api/v1/status/ping", "/api/v2/ping"} {
		response, _ = http.Get(server.URL + path)
		if body := readBody(t, response); body != "Pong" {
			t.Fatalf("%s answered %q", path, body)
		}
	}
	preflight, _ := http.NewRequest(http.MethodOptions, server.URL+"/api/v2/commit", nil)
	response, _ = http.DefaultClient.Do(preflight)
	if response.StatusCode != http.StatusOK || response.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("CORS preflight")
	}
}

// TestVersion1Routes_AnswerLikeTheirSuccessors checks that every /api/v1
// route answers like its /api/v2 successor and is marked as deprecated.
func TestVersion1Routes_AnswerLikeTheirSuccessors(t *testing.T) {
	server, _ := newTestServer(t)
	readBody(t, send(t, server, http.MethodPost, "/api/v2/commit", `{"events":[{"source":"s","subject":"/a/1","type":"t","data":{"x":1}}]}`))
	pairs := [][3]string{
		{http.MethodPost, "/api/v1/stream", "/api/v2/fetch"},
		{http.MethodPost, "/api/v1/q", "/api/v2/query"},
		{http.MethodPost, "/api/v1/query", "/api/v2/query"},
		{http.MethodGet, "/api/v1/status/audit", "/api/v2/admin/audit"},
		{http.MethodGet, "/api/v1/subjects", "/api/v2/subjects"},
		{http.MethodGet, "/api/v1/types", "/api/v2/types"},
		{http.MethodGet, "/api/v1/backup/create", "/api/v2/admin/backup"},
		{http.MethodGet, "/api/v1/schema/get", "/api/v2/schemas"},
		{http.MethodGet, "/api/v1/status/ping", "/api/v2/ping"},
	}
	bodies := map[string]string{
		"/api/v1/stream": `{"subject":"/"}`,
		"/api/v1/q":      `{"query":"STREAM e FROM events MAP COUNT()"}`,
		"/api/v1/query":  `{"query":"STREAM e FROM events MAP COUNT()"}`,
	}
	for _, pair := range pairs {
		v1 := send(t, server, pair[0], pair[1], bodies[pair[1]])
		v2 := send(t, server, pair[0], pair[2], bodies[pair[1]])
		if v1.Header.Get("Deprecation") != "true" || v1.Header.Get("Link") != "<"+pair[2]+`>; rel="successor-version"` {
			t.Errorf("%s: deprecation headers %q %q", pair[1], v1.Header.Get("Deprecation"), v1.Header.Get("Link"))
		}
		if v2.Header.Get("Deprecation") != "" {
			t.Errorf("%s: current route marked deprecated", pair[2])
		}
		if v1Body, v2Body := readBody(t, v1), readBody(t, v2); v1Body != v2Body {
			t.Errorf("%s and %s differ:\n%s\n%s", pair[1], pair[2], v1Body, v2Body)
		}
	}
}

// TestVersions_HaveNoForeignRoutes keeps the versions apart: the 2.0 names
// do not exist in /api/v1 and the 1.x names do not exist in /api/v2.
func TestVersions_HaveNoForeignRoutes(t *testing.T) {
	server, _ := newTestServer(t)
	for _, r := range [][2]string{
		{http.MethodPost, "/api/v1/fetch"},
		{http.MethodPost, "/api/v1/subscribe"},
		{http.MethodGet, "/api/v1/backup"},
		{http.MethodPost, "/api/v1/restore"},
		{http.MethodGet, "/api/v1/schemas"},
		{http.MethodPost, "/api/v1/schemas"},
		{http.MethodGet, "/api/v1/admin/backup"},
		{http.MethodPost, "/api/v1/import"},
		{http.MethodGet, "/api/v1/eventtypes"},
		{http.MethodPost, "/api/v2/stream"},
		{http.MethodPost, "/api/v2/observe"},
		{http.MethodPost, "/api/v2/q"},
		{http.MethodGet, "/api/v2/eventtypes"},
		{http.MethodGet, "/api/v2/status/ping"},
		{http.MethodGet, "/api/v2/status/audit"},
		{http.MethodGet, "/api/v2/backup"},
		{http.MethodGet, "/api/v2/backup/create"},
		{http.MethodPost, "/api/v2/backup/restore"},
		{http.MethodPost, "/api/v2/import"},
		{http.MethodPost, "/api/v2/restore"},
		{http.MethodPost, "/api/v2/erase"},
		{http.MethodPost, "/api/v2/index"},
		{http.MethodGet, "/api/v2/schema/get"},
		{http.MethodPost, "/api/v2/schema/register"},
		{http.MethodGet, "/api/v2/schemas/person"},
	} {
		response := send(t, server, r[0], r[1], `{}`)
		readBody(t, response)
		if response.StatusCode != http.StatusNotFound && response.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d, want no route", r[0], r[1], response.StatusCode)
		}
	}
}

func TestCommitFetchQueryAndErase(t *testing.T) {
	server, _ := newTestServer(t)
	response := send(t, server, http.MethodPost, "/api/v2/schemas", `{"type":"person","schema":{"properties":{"name":{"type":"string","required":true}}}}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatal(readBody(t, response))
	}
	response = send(t, server, http.MethodPost, "/api/v2/commit", `{"events":[{"source":"s","subject":"/person/1","type":"person","data":{"age":1}}]}`)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(readBody(t, response), "schema validation failed") {
		t.Fatal("schema not enforced")
	}
	response = send(t, server, http.MethodPost, "/api/v2/commit", `{"events":[{"source":"s","subject":"/person/1","type":"person","data":{"name":"Bruce"},"options":{"storeDataAsReference":true}}]}`)
	if body := readBody(t, response); response.StatusCode != http.StatusOK || !strings.Contains(body, "Bruce") || strings.Contains(body, `"ref"`) {
		t.Fatalf("commit: %d %s", response.StatusCode, body)
	}
	response = send(t, server, http.MethodPost, "/api/v2/commit", `{"events":[{"source":"s","subject":"/person/2","type":"t","data":{"a":1}}],"preconditions":[{"type":"isQueryResultTrue","payload":{"query":"STREAM e FROM events WHERE e.subject UNDER '/person' MAP COUNT() == 0"}}]}`)
	if response.StatusCode != http.StatusPreconditionFailed || !strings.Contains(readBody(t, response), `"type":"isQueryResultTrue"`) {
		t.Fatal("query precondition not enforced")
	}
	response = send(t, server, http.MethodPost, "/api/v2/fetch", `{"subject":"/person"}`)
	if body := readBody(t, response); response.Header.Get("Content-Type") != "application/x-ndjson" || !strings.Contains(body, "Bruce") {
		t.Fatalf("fetch: %s", body)
	}
	response = send(t, server, http.MethodPost, "/api/v2/admin/erase", `{"subject":"/person"}`)
	if body := readBody(t, response); body != "Erased 1 ref data files for subject /person" {
		t.Fatalf("erase: %s", body)
	}
	response = send(t, server, http.MethodPost, "/api/v2/query", `{"query":"STREAM e FROM events MAP { name: e.data.name }"}`)
	if body := readBody(t, response); body != "{\"name\":null}\n\n" {
		t.Fatalf("query after erasure: %q", body)
	}
	response = send(t, server, http.MethodGet, "/api/v2/status", "")
	var status StatusResponse
	json.NewDecoder(response.Body).Decode(&status)
	if status.Events.Count != 1 || status.Storage.Format != "current" || !status.Storage.IsReady {
		t.Fatalf("status %+v", status)
	}
}

func TestSubscribe_StreamsHistoryNewEventsAndHeartbeats(t *testing.T) {
	server, _ := newTestServer(t)
	readBody(t, send(t, server, http.MethodPost, "/api/v2/commit", `{"events":[{"source":"s","subject":"/o/1","type":"t","data":{"n":1}}]}`))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, path := range []string{"/api/v2/subscribe", "/api/v1/observe"} {
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path, strings.NewReader(`{"subject":"/o","interval":0.1}`))
		request.Header.Set("Authorization", "Bearer secret")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatal("not an event stream")
		}
		lines := bufio.NewScanner(response.Body)
		expect := func(fragment string) {
			deadline := time.Now().Add(5 * time.Second)
			for lines.Scan() {
				if strings.Contains(lines.Text(), fragment) {
					return
				}
				if time.Now().After(deadline) {
					break
				}
			}
			t.Fatalf("%s: %q not received", path, fragment)
		}
		expect(`"subject":"/o/1"`)
		expect(`{"payload":""}`)
		readBody(t, send(t, server, http.MethodPost, "/api/v2/commit", `{"events":[{"source":"s","subject":"/o/2","type":"t","data":{"n":2}}]}`))
		expect(`"subject":"/o/2"`)
		response.Body.Close()
	}
}

func TestBackupAndRestore(t *testing.T) {
	source, _ := newTestServer(t)
	readBody(t, send(t, source, http.MethodPost, "/api/v2/commit", `{"events":[{"source":"s","subject":"/b/1","type":"t","data":{"n":1}}]}`))
	backup := send(t, source, http.MethodGet, "/api/v2/admin/backup", "")
	if backup.Header.Get("Content-Disposition") != "attachment; filename=genesisdb-backup.json" {
		t.Fatal("backup file name")
	}
	content := readBody(t, backup)
	target, _ := newTestServer(t)
	response := send(t, target, http.MethodPost, "/api/v2/admin/restore", content)
	if body := readBody(t, response); body != "Successfully imported 1 events" {
		t.Fatalf("restore: %s", body)
	}
	response = send(t, target, http.MethodPost, "/api/v2/admin/restore", content)
	if response.StatusCode != http.StatusConflict {
		t.Fatal("restore into a non-empty store accepted")
	}
}

// TestSubscribe_CatchesUpUnderTheReadBudget checks that a subscriber behind
// by more events than one selection may hold still receives everything, in
// order, and that a failing subscription ends with an explicit error event.
func TestSubscribe_CatchesUpUnderTheReadBudget(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := store.Open(t.TempDir(), store.Options{EvaluateGDBQLPrecondition: query.EvaluatePrecondition, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(s, Options{AuthToken: "secret", Logger: logger, MaxReadEvents: 3}).Routes())
	t.Cleanup(func() {
		server.Close()
		s.Close()
	})
	var events []string
	for position := 0; position < 8; position++ {
		events = append(events, fmt.Sprintf(`{"source":"s","subject":"/h/%d","type":"t","data":{"n":%d}}`, position, position))
	}
	readBody(t, send(t, server, http.MethodPost, "/api/v2/commit", `{"events":[`+strings.Join(events, ",")+`]}`))
	if response := send(t, server, http.MethodPost, "/api/v2/fetch", `{"subject":"/h"}`); response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("precondition: fetch must exceed the budget, got %d", response.StatusCode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v2/subscribe", strings.NewReader(`{"subject":"/h","interval":0.1}`))
	request.Header.Set("Authorization", "Bearer secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("subscription with a history beyond the budget rejected: %d", response.StatusCode)
	}
	lines := bufio.NewScanner(response.Body)
	received := []string{}
	deadline := time.Now().Add(10 * time.Second)
	for len(received) < 8 && lines.Scan() {
		line := lines.Text()
		if strings.Contains(line, "event: error") {
			t.Fatalf("subscription failed: %s", line)
		}
		for position := 0; position < 8; position++ {
			if strings.Contains(line, fmt.Sprintf(`"subject":"/h/%d"`, position)) {
				received = append(received, fmt.Sprintf("/h/%d", position))
			}
		}
		if time.Now().After(deadline) {
			break
		}
	}
	for position := 0; position < 8; position++ {
		if position >= len(received) || received[position] != fmt.Sprintf("/h/%d", position) {
			t.Fatalf("history incomplete or out of order: %v", received)
		}
	}
}

func TestSubscribe_EndsWithAnErrorEvent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := store.Open(t.TempDir(), store.Options{EvaluateGDBQLPrecondition: query.EvaluatePrecondition, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	// A byte budget no event fits in: the history of an empty subject is
	// fine, the first committed event fails every poll.
	server := httptest.NewServer(New(s, Options{AuthToken: "secret", Logger: logger, MaxReadBytes: 1}).Routes())
	t.Cleanup(func() {
		server.Close()
		s.Close()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v2/subscribe", strings.NewReader(`{"subject":"/k","interval":0.1}`))
	request.Header.Set("Authorization", "Bearer secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if err := s.Commit(context.Background(), []*core.Event{{Source: "s", Subject: "/k/1", Type: "t", Data: json.RawMessage(`{"n":1}`)}}); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(response.Body)
	sawError := false
	for lines.Scan() {
		if lines.Text() == "event: error" {
			sawError = true
		}
		if sawError && strings.Contains(lines.Text(), "read budget exceeded") {
			return
		}
	}
	t.Fatal("stream ended without an explicit error event")
}
