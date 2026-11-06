package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
	"github.com/genesisdb-io/genesisdb/v2/packages/storage"
)

func signedBackup(t *testing.T, signer *core.EventSigner) *core.Backup {
	t.Helper()
	source := openSigningStore(t, t.TempDir(), signer)
	defer source.Close()
	commitTestEvent(t, source, "/a", "t")
	commitTestEvent(t, source, "/b", "t")
	if err := source.Commit(context.Background(), []*core.Event{{Source: "test", Subject: "/p", Type: "t", Data: json.RawMessage(`{"x":1}`), Options: core.EventOptions{StoreDataAsReference: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Erase(context.Background(), "/p"); err != nil {
		t.Fatal(err)
	}
	backup, err := source.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(backup.SystemEvents) == 0 || backup.SystemEvents[0].Signature == "" {
		t.Fatal("erasure wrote no signed system event")
	}
	return backup
}

func assertRestoreRejectedUnchanged(t *testing.T, target *Store, directory string, backup *core.Backup) {
	t.Helper()
	before := snapshotDirectory(t, filepath.Join(directory, layout.EventsDirectory))
	err := target.Restore(context.Background(), backup)
	if !errors.Is(err, core.ErrInvalidBackup) {
		t.Fatalf("tampered backup restored: %v", err)
	}
	if after := snapshotDirectory(t, filepath.Join(directory, layout.EventsDirectory)); after != before {
		t.Fatal("rejected restore changed the event logs")
	}
	if count, _ := target.CountEvents(); count != 0 {
		t.Fatalf("rejected restore left %d events", count)
	}
}

// Regression: a restore re-signed signed events whose hash changed, so a
// backup tampered with in transit came out validly signed by the server key.
func TestSigning_RestoreRejectsTamperedSignedEvents(t *testing.T) {
	signer := newTestEventSigner(t, 1)
	tamper := map[string]func(backup *core.Backup){
		"payload changed": func(backup *core.Backup) { backup.Events[0].Data = json.RawMessage(`{"tampered":true}`) },
		"subject changed": func(backup *core.Backup) { backup.Events[1].Subject = "/c" },
		"event removed":   func(backup *core.Backup) { backup.Events = backup.Events[1:] },
		"events swapped": func(backup *core.Backup) {
			backup.Events[0], backup.Events[1] = backup.Events[1], backup.Events[0]
		},
		"signature forged": func(backup *core.Backup) {
			backup.Events[0].Signature = newTestEventSigner(t, 2).SignHash(backup.Events[0].Hash)
		},
		"hash recomputed": func(backup *core.Backup) {
			event := backup.Events[len(backup.Events)-1]
			event.Data = json.RawMessage(`{"tampered":true}`)
			event.Hash = core.HashEvent(event)
		},
		"system event changed": func(backup *core.Backup) { backup.SystemEvents[0].Subject = "/x" },
	}
	for name, change := range tamper {
		t.Run(name, func(t *testing.T) {
			backup := signedBackup(t, signer)
			change(backup)
			for _, targetSigner := range []*core.EventSigner{nil, signer} {
				directory := t.TempDir()
				target := openSigningStore(t, directory, targetSigner)
				assertRestoreRejectedUnchanged(t, target, directory, backup)
				target.Close()
			}
		})
	}
}

// A key added to the backup itself only detects damage. Pinned keys reject
// a backup that an attacker re-signed with a key of their own.
func TestSigning_RestoreChecksPinnedKeys(t *testing.T) {
	signer, attacker := newTestEventSigner(t, 1), newTestEventSigner(t, 9)
	backup := signedBackup(t, signer)
	event := backup.Events[len(backup.Events)-1]
	event.Data = json.RawMessage(`{"tampered":true}`)
	event.Hash = core.HashEvent(event)
	event.Signature = attacker.SignHash(event.Hash)
	backup.SigningKeys = append(backup.SigningKeys, attacker.PublicKey())

	directory := t.TempDir()
	pinned := openSigningStore(t, directory, nil, signer.PublicKey())
	defer pinned.Close()
	assertRestoreRejectedUnchanged(t, pinned, directory, backup)
}

func TestSigning_RestoreOfUntamperedBackupKeepsEverySignature(t *testing.T) {
	signer := newTestEventSigner(t, 1)
	backup := signedBackup(t, signer)
	target := openSigningStore(t, t.TempDir(), nil, signer.PublicKey())
	defer target.Close()
	if err := target.Restore(context.Background(), backup); err != nil {
		t.Fatal(err)
	}
	report, err := target.VerifyHashChain(context.Background())
	if err != nil || !report.IsValid() || report.SignedEvents != 3 || report.SystemSignatures.Signed != 2 || !report.IsTrustPinned {
		t.Fatalf("%+v %v", report, err)
	}
}

// Regression: a migrated 1.x store has unsigned legacy events before signed
// ones. A restore gives the legacy events the current hash version, which
// changes every later hash. The restore must succeed and drop only the
// signatures whose hash changed, not fail and leave the store unavailable.
func TestSigning_RestoreOfMigratedStoreDropsShiftedSignatures(t *testing.T) {
	directory := newTestDirectory(t, layout.FormatLegacy)
	legacy := openTestStore(t, directory, testStoreOptions{})
	commitTestEvent(t, legacy, "/old", "t")
	legacy.Close()
	signer := newTestEventSigner(t, 1)
	source := openSigningStore(t, directory, signer)
	defer source.Close()
	commitTestEvent(t, source, "/new", "t")
	backup, err := source.Backup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target := openSigningStore(t, t.TempDir(), signer)
	defer target.Close()
	if err := target.Restore(context.Background(), backup); err != nil {
		t.Fatal(err)
	}
	if report := verifyChain(t, target); report.Problem != "" || report.Signed != 0 || report.Unsigned != 2 {
		t.Fatalf("%+v", report)
	}
}

// Signatures of the erasure audit trail are verified like events.
func TestSigning_AuditVerifiesSystemEventSignatures(t *testing.T) {
	directory := t.TempDir()
	signer := newTestEventSigner(t, 1)
	s := openSigningStore(t, directory, signer)
	if err := s.Commit(context.Background(), []*core.Event{{Source: "test", Subject: "/p", Type: "t", Data: json.RawMessage(`{"x":1}`), Options: core.EventOptions{StoreDataAsReference: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Erase(context.Background(), "/p"); err != nil {
		t.Fatal(err)
	}
	report, err := s.VerifyHashChain(context.Background())
	if err != nil || !report.IsValid() || report.SystemSignatures.Signed != 2 || report.IsTrustPinned {
		t.Fatalf("%+v %v", report, err)
	}
	s.Close()

	path := filepath.Join(directory, layout.EventsDirectory, layout.FormatCurrent.SystemEventLogFile())
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(content), "\n")
	event, err := storage.DecodeEventLine([]byte(lines[1]))
	if err != nil {
		t.Fatal(err)
	}
	lines[1] = strings.Replace(lines[1], event.Subject, "/tampered", 1)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	offline, err := VerifyStoppedDirectory(context.Background(), directory, []core.SigningPublicKey{signer.PublicKey()}, nil)
	if err != nil || !strings.Contains(offline.Problem, "System event "+event.ID) || !offline.IsTrustPinned {
		t.Fatalf("tampered system event not detected: %+v %v", offline, err)
	}

	// A revoked key is not trusted for system events, which revocation
	// points do not order.
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	revoked, err := VerifyStoppedDirectory(context.Background(), directory, nil, []core.SigningKeyRevocation{{KeyID: signer.PublicKey().KeyID}})
	if err != nil || !revoked.IsValid() || revoked.SystemSignatures.Revoked != 2 || revoked.SystemSignatures.Signed != 0 {
		t.Fatalf("revoked system signatures: %+v %v", revoked, err)
	}
}

// Fault injection: recording the signing keys of a backup fails after the
// logs were written. The restore must leave the directory unavailable
// instead of serving events whose keys are unrecorded, and a fresh restore
// into an empty directory must succeed.
func TestSigning_RestoreFailsWhenSigningKeysCannotBeRecorded(t *testing.T) {
	signer := newTestEventSigner(t, 1)
	backup := signedBackup(t, signer)
	directory := t.TempDir()
	faults := &switchableFaults{}
	target, err := Open(directory, Options{CacheSize: -1, Logger: discardLogger(), Faults: faults.injector()})
	if err != nil {
		t.Fatal(err)
	}
	var restoreErr error
	faults.during(failAtNthCall("replace.write:"+layout.SigningKeysFile, 1), func() {
		restoreErr = target.Restore(context.Background(), backup)
	})
	if restoreErr == nil || !strings.Contains(restoreErr.Error(), "restore signing keys failed") {
		t.Fatalf("restore error: %v", restoreErr)
	}
	if _, err := target.Fetch(context.Background(), "/a", FetchOptions{}); err == nil {
		t.Fatal("store serves reads after a failed restore")
	}
	target.Close()
	if _, err := os.Stat(filepath.Join(directory, layout.SigningKeysFile)); !os.IsNotExist(err) {
		t.Fatalf("partial %s left behind: %v", layout.SigningKeysFile, err)
	}
	if _, err := Open(directory, Options{Logger: discardLogger()}); err == nil {
		t.Fatal("restart serves a directory with an interrupted restore")
	}

	fresh := openSigningStore(t, t.TempDir(), nil)
	defer fresh.Close()
	if err := fresh.Restore(context.Background(), backup); err != nil {
		t.Fatal(err)
	}
	if report := verifyChain(t, fresh); report.Problem != "" || report.Signed != 3 {
		t.Fatalf("%+v", report)
	}
}
