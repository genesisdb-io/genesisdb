package store

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
	"github.com/genesisdb-io/genesisdb/v2/packages/storage"
)

func newTestEventSigner(t testing.TB, seed byte) *core.EventSigner {
	t.Helper()
	seedBytes := make([]byte, ed25519.SeedSize)
	seedBytes[0] = seed
	signer, err := core.NewEventSigner(ed25519.NewKeyFromSeed(seedBytes))
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func openSigningStore(t *testing.T, directory string, signer *core.EventSigner, trusted ...core.SigningPublicKey) *Store {
	t.Helper()
	s, err := Open(directory, Options{CacheSize: -1, Logger: discardLogger(), Signer: signer, TrustedSigningKeys: trusted})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

// signatureCounts keeps the assertions short.
type signatureCounts struct {
	Problem                      string
	Signed, Unsigned, UnknownKey int
}

func verifyChain(t *testing.T, s *Store) signatureCounts {
	t.Helper()
	report, err := s.VerifyHashChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return signatureCounts{Problem: report.Problem, Signed: report.SignedEvents, Unsigned: report.UnsignedEvents, UnknownKey: report.UnknownKeyEvents}
}

// snapshotDirectory returns the names, sizes and contents of every file.
func snapshotDirectory(t *testing.T, directory string) string {
	t.Helper()
	var snapshot strings.Builder
	err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		fmt.Fprintf(&snapshot, "%s %d %x\n", path, info.Size(), content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.String()
}

func TestSigning_CommittedEventsAreSignedAndVerified(t *testing.T) {
	ctx := context.Background()
	signer := newTestEventSigner(t, 1)
	directory := t.TempDir()
	s := openSigningStore(t, directory, signer)
	defer s.Close()
	batch := make([]*core.Event, 100) // above the parallel signing threshold
	for position := range batch {
		batch[position] = newTestEvent(fmt.Sprintf("/s/%d", position), "t", map[string]any{"n": position})
	}
	batch = append(batch, &core.Event{Source: "test", Subject: "/s/ref", Type: "t", Data: json.RawMessage(`{"name":"Bruce"}`), Options: core.EventOptions{StoreDataAsReference: true}})
	if err := s.Commit(ctx, batch); err != nil {
		t.Fatal(err)
	}
	keys, _ := core.NewSigningKeySet([]core.SigningPublicKey{signer.PublicKey()})
	for _, event := range readEventLogDirectly(t, s) {
		if keys.VerifySignature(event) != core.SignatureValid {
			t.Fatalf("event %s: persisted signature does not verify", event.ID)
		}
	}
	fetched, err := s.Fetch(ctx, "/s/ref", FetchOptions{})
	if err != nil || len(fetched) != 1 || keys.VerifySignature(fetched[0]) != core.SignatureValid {
		t.Fatalf("fetched signature: %v %v", fetched, err)
	}
	if report := verifyChain(t, s); report.Problem != "" || report.Signed != 101 || report.Unsigned != 0 {
		t.Fatalf("%+v", report)
	}
	// Erasure replaces the reference payload, the hash and the signature
	// stay valid.
	if _, err := s.Erase(ctx, "/s/ref"); err != nil {
		t.Fatal(err)
	}
	if report := verifyChain(t, s); report.Problem != "" || report.Signed != 101 {
		t.Fatalf("after erasure: %+v", report)
	}
	recorded, err := s.SigningKeys()
	if err != nil || len(recorded) != 1 || recorded[0] != signer.PublicKey() {
		t.Fatalf("recorded keys %v %v", recorded, err)
	}
}

func TestSigning_CallerCannotSupplyASignature(t *testing.T) {
	for _, signer := range []*core.EventSigner{nil, newTestEventSigner(t, 1)} {
		s := openSigningStore(t, t.TempDir(), signer)
		event := newTestEvent("/a", "t", map[string]any{})
		event.Signature = core.SignaturePrefix + "forged"
		if err := s.Commit(context.Background(), []*core.Event{event}); err != nil {
			t.Fatal(err)
		}
		persisted := readEventLogDirectly(t, s)[0]
		if signer == nil && persisted.Signature != "" || signer != nil && persisted.Signature != signer.SignHash(persisted.Hash) {
			t.Fatalf("persisted signature %q", persisted.Signature)
		}
		s.Close()
	}
}

func TestSigning_WithoutKeyRecordsAreByteForByteUnchanged(t *testing.T) {
	directory := t.TempDir()
	s := openSigningStore(t, directory, nil)
	commitTestEvent(t, s, "/a", "t")
	s.Close()
	content, err := os.ReadFile(filepath.Join(directory, layout.EventsDirectory, layout.FormatCurrent.MainEventLogFile()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "signature") {
		t.Fatalf("unsigned store wrote a signature field: %s", content)
	}
	if _, err := os.Stat(filepath.Join(directory, layout.SigningKeysFile)); !os.IsNotExist(err) {
		t.Fatalf("unsigned store created %s: %v", layout.SigningKeysFile, err)
	}
}

// Rotation: old events keep the signature of the key that committed them
// and stay verifiable, because every key is recorded.
func TestSigning_KeyRotationKeepsHistoryVerifiable(t *testing.T) {
	directory := t.TempDir()
	first, second := newTestEventSigner(t, 1), newTestEventSigner(t, 2)
	s := openSigningStore(t, directory, nil)
	commitTestEvent(t, s, "/a/0", "t")
	s.Close()
	s = openSigningStore(t, directory, first)
	commitTestEvent(t, s, "/a/1", "t")
	s.Close()
	s = openSigningStore(t, directory, second)
	commitTestEvent(t, s, "/a/2", "t")
	if report := verifyChain(t, s); report.Problem != "" || report.Signed != 2 || report.Unsigned != 1 {
		t.Fatalf("%+v", report)
	}
	events := readEventLogDirectly(t, s)
	firstID, _ := core.SignatureKeyID(events[1].Signature)
	secondID, _ := core.SignatureKeyID(events[2].Signature)
	if firstID != first.PublicKey().KeyID || secondID != second.PublicKey().KeyID {
		t.Fatal("signatures do not name the committing key")
	}
	recorded, _ := s.SigningKeys()
	if len(recorded) != 2 {
		t.Fatalf("recorded %v", recorded)
	}
	s.Close()
	// Pinning only the second key: the first key's signatures are reported
	// as untrusted, not as valid.
	s = openSigningStore(t, directory, nil, second.PublicKey())
	defer s.Close()
	if report := verifyChain(t, s); report.Problem != "" || report.Signed != 1 || report.UnknownKey != 1 {
		t.Fatalf("pinned: %+v", report)
	}
}

// A writer of the directory who re-signs an event with a key added to
// signing-keys.json is caught by pinned trusted keys.
func TestSigning_ForgedSignatureIsDetected(t *testing.T) {
	directory := t.TempDir()
	signer := newTestEventSigner(t, 1)
	s := openSigningStore(t, directory, signer)
	commitTestEvent(t, s, "/a", "t")
	commitTestEvent(t, s, "/b", "t")
	s.Close()

	path := filepath.Join(directory, layout.EventsDirectory, layout.FormatCurrent.MainEventLogFile())
	content, _ := os.ReadFile(path)
	lines := strings.Split(string(content), "\n")
	event, err := storage.DecodeEventLine([]byte(lines[2]))
	if err != nil {
		t.Fatal(err)
	}
	// Flip one signature byte.
	broken := []byte(event.Signature)
	broken[len(broken)-1] ^= 1
	if broken[len(broken)-1] > 'f' || (broken[len(broken)-1] > '9' && broken[len(broken)-1] < 'a') {
		broken[len(broken)-1] = '0'
	}
	lines[2] = strings.Replace(lines[2], event.Signature, string(broken), 1)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	s = openSigningStore(t, directory, nil)
	if report := verifyChain(t, s); !strings.Contains(report.Problem, "invalid signature") {
		t.Fatalf("tampered signature not detected: %+v", report)
	}
	s.Close()

	// Re-signed by an attacker key that is recorded in the directory: the
	// recorded keys accept it, pinned keys do not.
	attacker := newTestEventSigner(t, 9)
	lines[2] = strings.Replace(lines[2], string(broken), attacker.SignHash(event.Hash), 1)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	s = openSigningStore(t, directory, attacker)
	s.Close()
	s = openSigningStore(t, directory, nil, signer.PublicKey())
	defer s.Close()
	if report := verifyChain(t, s); report.Problem != "" || report.UnknownKey != 1 || report.Signed != 1 {
		t.Fatalf("pinned keys must not trust the attacker: %+v", report)
	}
	if report, err := VerifyStoppedDirectory(context.Background(), directory, nil); err == nil {
		t.Fatalf("directory is in use, offline verify must refuse: %+v", report)
	}
}

func TestSigning_OfflineVerifyIsReadOnly(t *testing.T) {
	directory := t.TempDir()
	signer := newTestEventSigner(t, 1)
	s := openSigningStore(t, directory, signer)
	commitTestEvent(t, s, "/a", "t")
	s.Close()
	before := snapshotDirectory(t, directory)
	report, err := VerifyStoppedDirectory(context.Background(), directory, []core.SigningPublicKey{signer.PublicKey()})
	if err != nil || !report.IsValid() || report.SignedEvents != 1 {
		t.Fatalf("%+v %v", report, err)
	}
	if after := snapshotDirectory(t, directory); after != before {
		t.Fatal("offline verification changed the directory")
	}
}

func TestSigning_BackupAndRestoreKeepSignatures(t *testing.T) {
	ctx := context.Background()
	signer := newTestEventSigner(t, 1)
	source := openSigningStore(t, t.TempDir(), signer)
	defer source.Close()
	commitTestEvent(t, source, "/a", "t")
	if err := source.Commit(ctx, []*core.Event{{Source: "test", Subject: "/p", Type: "t", Data: json.RawMessage(`{"x":1}`), Options: core.EventOptions{StoreDataAsReference: true}}}); err != nil {
		t.Fatal(err)
	}
	backup, err := source.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(backup.SigningKeys) != 1 || !backup.Extended {
		t.Fatalf("backup keys %v", backup.SigningKeys)
	}
	var encoded strings.Builder
	if err := backup.EncodeJSON(&encoded); err != nil {
		t.Fatal(err)
	}
	decoded, err := core.DecodeBackup([]byte(encoded.String()))
	if err != nil {
		t.Fatal(err)
	}
	// Restored without any key: signatures and the recorded key survive.
	target := openSigningStore(t, t.TempDir(), nil)
	defer target.Close()
	if err := target.Restore(ctx, decoded); err != nil {
		t.Fatal(err)
	}
	restored := readEventLogDirectly(t, target)
	original := readEventLogDirectly(t, source)
	for position := range original {
		if restored[position].Signature != original[position].Signature {
			t.Fatalf("event %d lost its signature", position)
		}
	}
	if report := verifyChain(t, target); report.Problem != "" || report.Signed != 2 {
		t.Fatalf("%+v", report)
	}
}

// A 1.x backup has neither signatures nor keys. Restoring it with a signing
// key must not claim that the key committed that history.
func TestSigning_RestoreNeverSignsUnsignedHistory(t *testing.T) {
	ctx := context.Background()
	source := openSigningStore(t, t.TempDir(), nil)
	defer source.Close()
	commitTestEvent(t, source, "/a", "t")
	backup, err := source.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := openSigningStore(t, t.TempDir(), newTestEventSigner(t, 1))
	defer target.Close()
	if err := target.Restore(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if report := verifyChain(t, target); report.Problem != "" || report.Signed != 0 || report.Unsigned != 1 {
		t.Fatalf("%+v", report)
	}
}

// Legacy directories kept in the 1.x format must stay readable by 1.x, so
// they are never signed.
func TestSigning_LegacyFormatIsNeverSigned(t *testing.T) {
	directory := newTestDirectory(t, layout.FormatLegacy)
	s, err := Open(directory, Options{CacheSize: -1, FormatPolicy: KeepExistingFormat, Logger: discardLogger(), Signer: newTestEventSigner(t, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	commitTestEvent(t, s, "/a", "t")
	if readEventLogDirectly(t, s)[0].Signature != "" {
		t.Fatal("legacy record signed")
	}
	if _, isSigning := s.ActiveSigningKey(); isSigning {
		t.Fatal("legacy store reports signing")
	}
	if _, err := os.Stat(filepath.Join(directory, layout.SigningKeysFile)); !os.IsNotExist(err) {
		t.Fatal("legacy store records a signing key")
	}
}

// Migration from 1.x with a key: history stays unsigned, new events are
// signed, the chain verifies across the boundary.
func TestSigning_MigratedLegacyDirectorySignsNewEventsOnly(t *testing.T) {
	directory := newTestDirectory(t, layout.FormatLegacy)
	legacy := openTestStore(t, directory, testStoreOptions{})
	commitTestEvent(t, legacy, "/old", "t")
	legacy.Close()
	signer := newTestEventSigner(t, 1)
	s, err := Open(directory, Options{CacheSize: -1, Logger: discardLogger(), Signer: signer})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	commitTestEvent(t, s, "/new", "t")
	if report := verifyChain(t, s); report.Problem != "" || report.Signed != 1 || report.Unsigned != 1 {
		t.Fatalf("%+v", report)
	}
}

// Fault injection: a crash after the WAL publish replays signed events
// with their signatures, and a failure to record the key stops the open
// before anything is signed.
func TestSigning_RecoveryReplaysSignedBatches(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	signer := newTestEventSigner(t, 1)
	faults := &switchableFaults{}
	s, err := Open(directory, Options{CacheSize: -1, Logger: discardLogger(), Signer: signer, Faults: faults.injector()})
	if err != nil {
		t.Fatal(err)
	}
	var commitErr error
	faults.during(failAtNthCall("events.write", 1), func() {
		commitErr = s.Commit(ctx, []*core.Event{newTestEvent("/a", "t", map[string]any{})})
	})
	if commitErr == nil {
		t.Fatal("injected fault ignored")
	}
	s.Close()
	s = openSigningStore(t, directory, signer)
	defer s.Close()
	if report := verifyChain(t, s); report.Problem != "" || report.Signed != 1 {
		t.Fatalf("replayed batch: %+v", report)
	}

	failing := t.TempDir()
	_, err = Open(failing, Options{Logger: discardLogger(), Signer: signer, Faults: failAtNthCall("replace.write:"+layout.SigningKeysFile, 1)})
	if err == nil {
		t.Fatal("open succeeded although the signing key could not be recorded")
	}
}

func BenchmarkCommitBatchSigned(b *testing.B) {
	s, err := Open(b.TempDir(), Options{Logger: discardLogger(), Signer: newTestEventSigner(b, 1)})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	b.ReportAllocs()
	for b.Loop() {
		events := make([]*core.Event, 10)
		for position := range events {
			events[position] = newTestEvent("/benchmark/1", "created", map[string]any{"n": position})
		}
		if err := s.Commit(context.Background(), events); err != nil {
			b.Fatal(err)
		}
	}
}
