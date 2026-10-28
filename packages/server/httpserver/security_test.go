package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReferencePayloadsUseSchemaAndSizeChecks(t *testing.T) {
	server, _ := newTestServer(t)
	readBody(t, send(t, server, http.MethodPost, "/api/v2/schemas", `{"type":"person","schema":{"properties":{"name":{"type":"string","required":true}}}}`))
	for _, body := range []string{
		`{"events":[{"source":"s","subject":"/p/1","type":"person","ref":{"age":1}}]}`,
		`{"events":[{"source":"s","subject":"/p/1","type":"person","ref":{"name":"` + strings.Repeat("x", 65536) + `"}}]}`,
	} {
		response := send(t, server, http.MethodPost, "/api/v2/commit", body)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid ref accepted: %d %s", response.StatusCode, readBody(t, response))
		}
		readBody(t, response)
	}
	response := send(t, server, http.MethodPost, "/api/v2/commit", `{"events":[{"source":"s","subject":"/p/1","type":"person","ref":{"name":"Bruce"},"options":{"storeDataAsReference":true}}]}`)
	if body := readBody(t, response); response.StatusCode != http.StatusOK || !strings.Contains(body, "Bruce") {
		t.Fatalf("combined flags lost data: %d %s", response.StatusCode, body)
	}
}

func TestBodyLimitsAndTrailingJSON(t *testing.T) {
	_, s := newTestServer(t)
	handler := New(s, Options{AuthToken: "admin", MaxRequestBytes: 64, MaxRestoreBytes: 64}).Routes()
	for _, path := range []string{"/api/v2/commit", "/api/v2/admin/restore", "/api/v1/commit", "/api/v1/backup/restore"} {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"events":[],"padding":"`+strings.Repeat("x", 128)+`"}`))
		request.Header.Set("Authorization", "Bearer admin")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v2/fetch", strings.NewReader(`{} {}`))
	request.Header.Set("Authorization", "Bearer admin")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatal("multiple JSON values accepted")
	}
}

func TestLeastPrivilegeTokensApplyToBothVersions(t *testing.T) {
	_, s := newTestServer(t)
	handler := New(s, Options{AuthToken: "admin", ReadToken: "reader", WriteToken: "writer"}).Routes()
	for _, token := range []string{"reader", "writer"} {
		for _, path := range []string{
			"/api/v2/admin/erase", "/api/v2/admin/restore", "/api/v2/admin/index", "/api/v2/schemas",
			"/api/v1/erase", "/api/v1/backup/restore", "/api/v1/index", "/api/v1/schema/register",
		} {
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("%s %s: %d", token, path, response.Code)
			}
		}
	}
	for _, token := range []string{"reader", "writer"} {
		for _, path := range []string{"/api/v2/admin/backup", "/api/v2/admin/audit", "/api/v1/backup/create", "/api/v1/status/audit"} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("%s %s: %d", token, path, response.Code)
			}
		}
		for _, path := range []string{"/api/v2/query", "/api/v1/q"} {
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"query":"STREAM e FROM events MAP COUNT()"}`))
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("%s: read denied: %d %s", path, response.Code, response.Body)
			}
		}
	}
	for _, path := range []string{"/api/v2/commit", "/api/v1/commit"} {
		for token, want := range map[string]int{"reader": http.StatusForbidden, "writer": http.StatusOK} {
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"events":[{"source":"s","subject":"/a/1","type":"t","data":{}}]}`))
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != want {
				t.Fatalf("%s with %s token: %d, want %d", path, token, response.Code, want)
			}
		}
	}
}

// TestCommit_RejectsInvalidUTF8 covers the HTTP path: the JSON decoder
// accepts invalid UTF-8 inside strings, so the store validation must turn
// it into a client error before anything is written.
func TestCommit_RejectsInvalidUTF8(t *testing.T) {
	server, s := newTestServer(t)
	defer server.Close()
	for name, body := range map[string]string{
		"subject": "{\"events\":[{\"source\":\"app\",\"subject\":\"/u/\xff\",\"type\":\"t\",\"data\":{}}]}",
		"type":    "{\"events\":[{\"source\":\"app\",\"subject\":\"/u/1\",\"type\":\"t\xff\",\"data\":{}}]}",
		"data":    "{\"events\":[{\"source\":\"app\",\"subject\":\"/u/1\",\"type\":\"t\",\"data\":{\"a\":\"\xff\"}}]}",
	} {
		response := send(t, server, http.MethodPost, "/api/v2/commit", body)
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", name, response.StatusCode, readBody(t, response))
		}
		response.Body.Close()
	}
	count, err := s.CountEvents()
	if err != nil || count != 0 {
		t.Fatalf("events written: %d %v", count, err)
	}
}
