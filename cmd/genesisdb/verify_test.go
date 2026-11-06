package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/store"
)

// --require-signatures must not pass on the strength of keys recorded in the
// directory, which a writer of the directory can extend.
func TestVerify_RequireSignaturesNeedsPinnedKeys(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 5
	signer, err := core.NewEventSigner(ed25519.NewKeyFromSeed(seed))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	s, err := store.Open(directory, store.Options{Logger: logger, Signer: signer})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(context.Background(), []*core.Event{{Source: "s", Subject: "/a", Type: "t", Data: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if code := runVerify([]string{"-data-dir", directory, "-trusted-keys", ""}, logger); code != 0 {
		t.Fatalf("plain verify: exit %d", code)
	}
	if code := runVerify([]string{"-data-dir", directory, "-trusted-keys", "", "-require-signatures"}, logger); code != 1 {
		t.Fatalf("require-signatures without pinned keys: exit %d", code)
	}
	pinned := signer.PublicKey().PublicKey
	if code := runVerify([]string{"-data-dir", directory, "-trusted-keys", pinned, "-require-signatures"}, logger); code != 0 {
		t.Fatalf("require-signatures with pinned key: exit %d", code)
	}
}
