package store

import (
	"context"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

func openRestoreTarget(t *testing.T, directory string, trusted []core.SigningPublicKey, revoked ...core.SigningKeyRevocation) *Store {
	t.Helper()
	s, err := Open(directory, Options{CacheSize: -1, Logger: discardLogger(), TrustedSigningKeys: trusted, RevokedSigningKeys: revoked})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Regression: a restore rejected every backup holding a signature of a
// revoked key, so the own backup of a store whose key was revoked could not
// be restored. Revoked signatures are now dropped, never kept, and the
// events are restored unsigned. Trusted signatures before the revocation
// point stay.
func TestSigning_RestoreDropsSignaturesOfRevokedKeys(t *testing.T) {
	signer := newTestEventSigner(t, 1)
	keyID := signer.PublicKey().KeyID
	backup := signedBackup(t, signer)
	last := len(backup.Events) - 1
	for name, test := range map[string]struct {
		revocation     core.SigningKeyRevocation
		keptSignatures int
	}{
		"whole key":               {core.SigningKeyRevocation{KeyID: keyID}, 0},
		"after the first event":   {core.SigningKeyRevocation{KeyID: keyID, ValidThroughHash: backup.Events[0].Hash}, 1},
		"system event of the key": {core.SigningKeyRevocation{KeyID: keyID, ValidThroughHash: backup.Events[last].Hash}, last + 1},
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			target := openRestoreTarget(t, directory, []core.SigningPublicKey{signer.PublicKey()}, test.revocation)
			defer target.Close()
			if err := target.Restore(context.Background(), backup); err != nil {
				t.Fatal(err)
			}
			restored, err := target.Backup(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			kept := 0
			for _, event := range restored.Events {
				if event.Signature != "" {
					kept++
				}
			}
			if kept != test.keptSignatures || len(restored.Events) != len(backup.Events) {
				t.Fatalf("kept %d signatures, expected %d, %d events", kept, test.keptSignatures, len(restored.Events))
			}
			for _, event := range restored.SystemEvents {
				if event.Signature != "" {
					t.Fatal("system event kept a signature of a revoked key")
				}
			}
			report, err := target.VerifyHashChain(context.Background())
			if err != nil || !report.IsValid() || report.RevokedEvents != 0 || report.SignedEvents != test.keptSignatures {
				t.Fatalf("audit after restore %+v %v", report, err)
			}
		})
	}
}

// A revoked signature is dropped, so tampering with its event is not
// detected by the signature, but every other check still applies.
func TestSigning_RestoreWithRevocationStillRejectsTampering(t *testing.T) {
	signer := newTestEventSigner(t, 1)
	backup := signedBackup(t, signer)
	backup.Events[1].Signature = newTestEventSigner(t, 2).SignHash(backup.Events[1].Hash)
	revocation := core.SigningKeyRevocation{KeyID: signer.PublicKey().KeyID, ValidThroughHash: backup.Events[0].Hash}
	directory := t.TempDir()
	target := openRestoreTarget(t, directory, []core.SigningPublicKey{signer.PublicKey()}, revocation)
	defer target.Close()
	assertRestoreRejectedUnchanged(t, target, directory, backup)
}

func TestSigning_RestoreAcceptsRevocationOfAnotherKey(t *testing.T) {
	signer, other := newTestEventSigner(t, 1), newTestEventSigner(t, 2)
	backup := signedBackup(t, signer)
	target := openRestoreTarget(t, t.TempDir(), []core.SigningPublicKey{signer.PublicKey()}, core.SigningKeyRevocation{KeyID: other.PublicKey().KeyID})
	defer target.Close()
	if err := target.Restore(context.Background(), backup); err != nil {
		t.Fatal(err)
	}
}

// Regression: with pinned trusted keys, a restore recorded every key the
// backup carried, so a backup could extend signing-keys.json at will.
func TestSigning_RestoreWithPinnedKeysRecordsPinnedKeysOnly(t *testing.T) {
	signer, extra := newTestEventSigner(t, 1), newTestEventSigner(t, 9)
	backup := signedBackup(t, signer)
	backup.SigningKeys = append(backup.SigningKeys, extra.PublicKey())

	pinned := openRestoreTarget(t, t.TempDir(), []core.SigningPublicKey{signer.PublicKey()})
	defer pinned.Close()
	if err := pinned.Restore(context.Background(), backup); err != nil {
		t.Fatal(err)
	}
	keys, err := pinned.SigningKeys()
	if err != nil || len(keys) != 1 || keys[0] != signer.PublicKey() {
		t.Fatalf("recorded keys %v %v", keys, err)
	}

	unpinned := openRestoreTarget(t, t.TempDir(), nil)
	defer unpinned.Close()
	if err := unpinned.Restore(context.Background(), backup); err != nil {
		t.Fatal(err)
	}
	if keys, err := unpinned.SigningKeys(); err != nil || len(keys) != 2 {
		t.Fatalf("without pinned keys every backup key is recorded: %v %v", keys, err)
	}
}
