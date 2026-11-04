package httpserver

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/query"
	"github.com/genesisdb-io/genesisdb/v2/packages/store"
)

func newSigningTestServer(t *testing.T) (*httptest.Server, *core.EventSigner) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	signer, err := core.NewEventSigner(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(t.TempDir(), store.Options{EvaluateGDBQLPrecondition: query.EvaluatePrecondition, Logger: logger, Signer: signer})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(s, Options{AuthToken: "secret", Logger: logger}).Routes())
	t.Cleanup(func() {
		server.Close()
		s.Close()
	})
	return server, signer
}

func TestSigning_Version2ReturnsSignaturesVersion1StaysFrozen(t *testing.T) {
	server, signer := newSigningTestServer(t)
	commit := send(t, server, http.MethodPost, "/api/v2/commit", `{"events":[{"source":"s","subject":"/a/1","type":"t","data":{"x":1}}]}`)
	var committed EventResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(readBody(t, commit))), &committed); err != nil {
		t.Fatal(err)
	}
	if committed.Signature != signer.SignHash(committed.Hash) {
		t.Fatalf("commit answer signature %q", committed.Signature)
	}
	for _, path := range []string{"/api/v2/fetch", "/api/v1/stream"} {
		body := readBody(t, send(t, server, http.MethodPost, path, `{"subject":"/a"}`))
		hasSignature := strings.Contains(body, `"signature":"`+committed.Signature+`"`)
		if hasSignature != strings.HasPrefix(path, "/api/v2") {
			t.Errorf("%s: signature present %v: %s", path, hasSignature, body)
		}
	}
	for _, path := range []string{"/api/v2/query", "/api/v1/q"} {
		body := readBody(t, send(t, server, http.MethodPost, path, `{"query":"STREAM e FROM events WHERE e.subject == '/a/1' MAP { * }"}`))
		if strings.Contains(body, "signature") != strings.HasPrefix(path, "/api/v2") {
			t.Errorf("%s: %q", path, body)
		}
	}
	v1Commit := readBody(t, send(t, server, http.MethodPost, "/api/v1/commit", `{"events":[{"source":"s","subject":"/b","type":"t","data":{}}]}`))
	if strings.Contains(v1Commit, "signature") {
		t.Errorf("/api/v1/commit answers with a signature: %s", v1Commit)
	}

	audit := readBody(t, send(t, server, http.MethodGet, "/api/v2/admin/audit", ""))
	if !strings.Contains(audit, "Audit passed") || !strings.Contains(audit, "Signatures: 2 valid, 0 unsigned") {
		t.Errorf("v2 audit %q", audit)
	}
	if v1Audit := readBody(t, send(t, server, http.MethodGet, "/api/v1/status/audit", "")); strings.Contains(v1Audit, "Signatures") {
		t.Errorf("v1 audit changed: %q", v1Audit)
	}

	var keys SigningKeysResponse
	if err := json.Unmarshal([]byte(readBody(t, send(t, server, http.MethodGet, "/api/v2/signing-keys", ""))), &keys); err != nil {
		t.Fatal(err)
	}
	if keys.Active != signer.PublicKey().KeyID || len(keys.Keys) != 1 || keys.Keys[0] != signer.PublicKey() {
		t.Fatalf("signing keys %+v", keys)
	}
	if response := send(t, server, http.MethodGet, "/api/v1/signing-keys", ""); response.StatusCode != http.StatusNotFound {
		t.Fatalf("frozen /api/v1 gained a route: %d", response.StatusCode)
	}
}

func TestSigning_UnsignedStoreAnswersAsBefore(t *testing.T) {
	server, _ := newTestServer(t)
	body := readBody(t, send(t, server, http.MethodPost, "/api/v2/commit", `{"events":[{"source":"s","subject":"/a","type":"t","data":{}}]}`))
	if strings.Contains(body, "signature") {
		t.Fatalf("unsigned commit answer: %s", body)
	}
	if audit := readBody(t, send(t, server, http.MethodGet, "/api/v2/admin/audit", "")); strings.Contains(audit, "Signatures") {
		t.Fatalf("unsigned audit: %q", audit)
	}
	keys := readBody(t, send(t, server, http.MethodGet, "/api/v2/signing-keys", ""))
	if strings.TrimSpace(keys) != `{"active":"","keys":[]}` {
		t.Fatalf("signing keys %s", keys)
	}
}
