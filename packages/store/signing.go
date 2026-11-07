package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
	"github.com/genesisdb-io/genesisdb/v2/packages/storage"
)

// signingKeysDocument is the content of signing-keys.json. The JSON field
// names are part of the on-disk format and of every backup.
type signingKeysDocument struct {
	Keys []core.SigningPublicKey `json:"keys"`
}

func (s *Store) signingKeysPath() string {
	return filepath.Join(s.dataDirectory, layout.SigningKeysFile)
}

// SigningKeys returns the public keys of every signing key that committed to
// the data directory, sorted by key ID. A directory that was never opened
// with a signing key has none.
//
// The list lives in the data directory, so whoever can write the directory
// can add a key. It identifies keys, it does not establish trust. Pin the
// trusted keys outside the directory (Options.TrustedSigningKeys).
func (s *Store) SigningKeys() ([]core.SigningPublicKey, error) {
	return ReadSigningKeys(s.dataDirectory)
}

// ReadSigningKeys reads the recorded public keys of a data directory without
// opening it, for offline verification.
func ReadSigningKeys(dataDirectory string) ([]core.SigningPublicKey, error) {
	content, err := os.ReadFile(filepath.Join(dataDirectory, layout.SigningKeysFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var document signingKeysDocument
	if err := json.Unmarshal(content, &document); err != nil {
		return nil, fmt.Errorf("%s: %w", layout.SigningKeysFile, err)
	}
	if _, err := core.NewSigningKeySet(document.Keys); err != nil {
		return nil, fmt.Errorf("%s: %w", layout.SigningKeysFile, err)
	}
	return document.Keys, nil
}

// registerSigningKeys adds public keys to signing-keys.json. Known keys are
// skipped, the file is only rewritten when a key is new, so a restart with
// the same key writes nothing.
func (s *Store) registerSigningKeys(keys []core.SigningPublicKey) error {
	if len(keys) == 0 {
		return nil
	}
	if _, err := core.NewSigningKeySet(keys); err != nil {
		return err
	}
	existing, err := s.SigningKeys()
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, key := range existing {
		known[key.KeyID] = true
	}
	merged := existing
	for _, key := range keys {
		if !known[key.KeyID] {
			known[key.KeyID] = true
			merged = append(merged, key)
		}
	}
	if len(merged) == len(existing) {
		return nil
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].KeyID < merged[j].KeyID })
	encoded, err := json.MarshalIndent(signingKeysDocument{Keys: merged}, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.ReplaceAtomically(s.signingKeysPath(), append(encoded, '\n'), nil, nil, s.faults)
}

// signatureTrust returns what signatures are checked against: the pinned
// trusted keys if any were configured, otherwise the keys recorded in the
// data directory, and the configured revocations. Without any key the set is
// empty, so signed events are counted as signed by an unknown key rather
// than silently accepted.
func (s *Store) signatureTrust() (*core.SignatureTrust, error) {
	return newSignatureTrust(s.dataDirectory, s.trustedSigningKeys, s.revokedSigningKeys)
}

// restoreSigningKeys returns the keys the signatures of a backup must verify
// with: the pinned trusted keys if configured, otherwise the keys the backup
// carries. Whoever can write the backup can add a key to it, so without
// pinned keys the check detects damage, not a deliberate rewrite.
func (s *Store) restoreSigningKeys(backup *core.Backup) (core.SigningKeySet, error) {
	if len(s.trustedSigningKeys) > 0 {
		return core.NewSigningKeySet(s.trustedSigningKeys)
	}
	return core.NewSigningKeySet(backup.SigningKeys)
}

// signingKeysToRecordFromBackup returns the keys of a backup that a restore
// adds to signing-keys.json. With pinned trusted keys only pinned keys are
// recorded: every kept signature verified with one of them, and a key the
// backup adds on its own would only extend the recorded list. Without pinned
// keys every key of the backup is recorded, since the audit verifies the
// restored signatures against the recorded keys.
func (s *Store) signingKeysToRecordFromBackup(backup *core.Backup) []core.SigningPublicKey {
	if len(s.trustedSigningKeys) == 0 {
		return backup.SigningKeys
	}
	pinned := map[core.SigningPublicKey]bool{}
	for _, key := range s.trustedSigningKeys {
		pinned[key] = true
	}
	var keys []core.SigningPublicKey
	for _, key := range backup.SigningKeys {
		if pinned[key] {
			keys = append(keys, key)
		}
	}
	return keys
}

func newSignatureTrust(dataDirectory string, trusted []core.SigningPublicKey, revocations core.SigningKeyRevocations) (*core.SignatureTrust, error) {
	keys := trusted
	isPinned := len(keys) > 0
	if !isPinned {
		recorded, err := ReadSigningKeys(dataDirectory)
		if err != nil {
			return nil, err
		}
		keys = recorded
	}
	set, err := core.NewSigningKeySet(keys)
	if err != nil {
		return nil, err
	}
	return &core.SignatureTrust{Keys: set, Revocations: revocations, IsPinned: isPinned}, nil
}

// VerifyStoppedDirectory checks the hash chain and every signature of a data
// directory that no process uses, without changing a byte. Signatures are
// checked against trusted if given, otherwise against the recorded keys,
// and signatures of revoked keys after their revocation point are not
// trusted.
func VerifyStoppedDirectory(ctx context.Context, dataDirectory string, trusted []core.SigningPublicKey, revoked []core.SigningKeyRevocation) (*storage.HashChainReport, error) {
	revocations, err := core.NewSigningKeyRevocations(revoked)
	if err != nil {
		return nil, err
	}
	lock, err := fsutil.LockDirectory(dataDirectory)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	trust, err := newSignatureTrust(dataDirectory, trusted, revocations)
	if err != nil {
		return nil, err
	}
	return storage.VerifyDirectory(ctx, dataDirectory, trust)
}

// RevokedSigningKeys returns the configured revocations sorted by key ID.
func (s *Store) RevokedSigningKeys() []core.SigningKeyRevocation {
	var revocations []core.SigningKeyRevocation
	for keyID, validThrough := range s.revokedSigningKeys {
		revocations = append(revocations, core.SigningKeyRevocation{KeyID: keyID, ValidThroughHash: validThrough})
	}
	sort.Slice(revocations, func(i, j int) bool { return revocations[i].KeyID < revocations[j].KeyID })
	return revocations
}

// ActiveSigningKey returns the public key new events are signed with, or
// false when signing is off.
func (s *Store) ActiveSigningKey() (core.SigningPublicKey, bool) {
	if s.signer == nil || s.format != layout.FormatCurrent {
		return core.SigningPublicKey{}, false
	}
	return s.signer.PublicKey(), true
}
