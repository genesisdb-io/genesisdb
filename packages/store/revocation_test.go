package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

func openRevokingStore(t *testing.T, directory string, signer *core.EventSigner, revoked ...core.SigningKeyRevocation) *Store {
	t.Helper()
	s, err := Open(directory, Options{CacheSize: -1, Logger: discardLogger(), Signer: signer, RevokedSigningKeys: revoked})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

// A key leaks after two events. Events up to the revocation point keep a
// trusted signature, everything the key signed later does not, including
// events an attacker appended with the leaked key.
func TestRevocation_CompromisedKeyAfterARevocationPoint(t *testing.T) {
	directory := t.TempDir()
	leaked, replacement := newTestEventSigner(t, 1), newTestEventSigner(t, 2)
	s := openRevokingStore(t, directory, leaked)
	commitTestEvent(t, s, "/a/1", "t")
	lastTrusted := commitTestEvent(t, s, "/a/2", "t")
	commitTestEvent(t, s, "/a/3", "t") // signed after the leak
	s.Close()

	revocation := core.SigningKeyRevocation{KeyID: leaked.PublicKey().KeyID, ValidThroughHash: lastTrusted.Hash}
	s = openRevokingStore(t, directory, replacement, revocation)
	commitTestEvent(t, s, "/a/4", "t")
	report, err := s.VerifyHashChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.IsValid() || report.SignedEvents != 3 || report.RevokedEvents != 1 {
		t.Fatalf("%+v", report)
	}
	if revoked := s.RevokedSigningKeys(); len(revoked) != 1 || revoked[0] != revocation {
		t.Fatalf("revocations %v", revoked)
	}
	s.Close()

	offline, err := VerifyStoppedDirectory(context.Background(), directory, nil, []core.SigningKeyRevocation{revocation})
	if err != nil || !offline.IsValid() || offline.RevokedEvents != 1 || offline.SignedEvents != 3 {
		t.Fatalf("offline %+v %v", offline, err)
	}
}

// Without a revocation point every signature of the key loses trust.
func TestRevocation_WholeKey(t *testing.T) {
	directory := t.TempDir()
	leaked := newTestEventSigner(t, 1)
	s := openRevokingStore(t, directory, leaked)
	commitTestEvent(t, s, "/a/1", "t")
	commitTestEvent(t, s, "/a/2", "t")
	s.Close()
	report, err := VerifyStoppedDirectory(context.Background(), directory, nil, []core.SigningKeyRevocation{{KeyID: leaked.PublicKey().KeyID}})
	if err != nil || !report.IsValid() || report.SignedEvents != 0 || report.RevokedEvents != 2 {
		t.Fatalf("%+v %v", report, err)
	}
}

// The revocation point doubles as a checkpoint: a rewritten history no
// longer contains it, which fails the audit.
func TestRevocation_MissingRevocationPointFailsTheAudit(t *testing.T) {
	directory := t.TempDir()
	leaked := newTestEventSigner(t, 1)
	s := openRevokingStore(t, directory, leaked)
	commitTestEvent(t, s, "/a/1", "t")
	s.Close()
	revocation := core.SigningKeyRevocation{KeyID: leaked.PublicKey().KeyID, ValidThroughHash: strings.Repeat("0f", 32)}
	s = openRevokingStore(t, directory, nil, revocation)
	defer s.Close()
	report, err := s.VerifyHashChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.Problem, "revocation point of key "+leaked.PublicKey().KeyID) {
		t.Fatalf("missing point not reported: %+v", report)
	}
}

func TestRevocation_RevokedKeyCannotSign(t *testing.T) {
	directory := t.TempDir()
	leaked := newTestEventSigner(t, 1)
	_, err := Open(directory, Options{Logger: discardLogger(), Signer: leaked, RevokedSigningKeys: []core.SigningKeyRevocation{{KeyID: leaked.PublicKey().KeyID}}})
	if !errors.Is(err, core.ErrInvalidSigningKey) {
		t.Fatalf("open with a revoked signing key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, layout.SigningKeysFile)); !os.IsNotExist(err) {
		t.Fatal("revoked key was recorded")
	}
	if _, err := Open(directory, Options{Logger: discardLogger(), RevokedSigningKeys: []core.SigningKeyRevocation{{KeyID: "short"}}}); err == nil {
		t.Fatal("malformed revocation accepted")
	}
	// The failed opens released the directory.
	openRevokingStore(t, directory, nil).Close()
}
