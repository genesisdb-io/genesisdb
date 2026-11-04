package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

// HashChainReport describes integrity checks, not proof against an attacker
// who can rewrite the data directory. Persist checkpoints externally for that.
type HashChainReport struct {
	CheckedEvents int
	LegacyEvents  int
	// SignedEvents carry a signature that verifies with a known key.
	SignedEvents int
	// UnsignedEvents carry no signature, for example events committed
	// before a signing key was configured.
	UnsignedEvents int
	// UnknownKeyEvents carry a signature of a key that is not in the key
	// set. Their signature proves nothing.
	UnknownKeyEvents int
	Subjects         []string
	Problem          string
}

func (report *HashChainReport) IsValid() bool { return report.Problem == "" }

// VerifyDirectory checks the hash chain and signatures of a stopped data
// directory without changing anything: no write-ahead log replay, no tail
// repair, no migration. A directory left by a crash may therefore report an
// incomplete final record that the next start would repair.
func VerifyDirectory(ctx context.Context, dataDirectory string, keys core.SigningKeySet) (*HashChainReport, error) {
	eventsDirectory := filepath.Join(dataDirectory, layout.EventsDirectory)
	format := layout.FormatCurrent
	if _, err := os.Stat(filepath.Join(eventsDirectory, format.MainEventLogFile())); errors.Is(err, os.ErrNotExist) {
		format = layout.FormatLegacy
	}
	if _, err := os.Stat(filepath.Join(eventsDirectory, format.MainEventLogFile())); err != nil {
		return nil, fmt.Errorf("no event log in %s: %w", eventsDirectory, err)
	}
	engine := &Engine{eventsDirectory: eventsDirectory, format: format}
	return engine.VerifyHashChainAndSignatures(ctx, keys)
}

func (engine *Engine) VerifyHashChain() (*HashChainReport, error) {
	return engine.VerifyHashChainContext(context.Background())
}

// VerifyHashChainContext checks the hash chain without checking signatures.
func (engine *Engine) VerifyHashChainContext(ctx context.Context) (*HashChainReport, error) {
	return engine.VerifyHashChainAndSignatures(ctx, nil)
}

// VerifyHashChainAndSignatures streams raw records in physical chain order.
// Clock corrections do not change chain order. Tagged records never use
// fallback algorithms and an upgraded chain cannot downgrade to an untagged
// record.
//
// With a key set every signature is verified as well, on all cores while the
// scan continues. A signature that does not verify with its key, or is
// malformed, is a problem. Signatures of keys outside the set and unsigned
// events are counted, not rejected. A nil key set skips signatures.
func (engine *Engine) VerifyHashChainAndSignatures(ctx context.Context, keys core.SigningKeySet) (*HashChainReport, error) {
	report := &HashChainReport{}
	subjects := map[string]bool{}
	previous := core.ZeroHash
	upgraded := false
	var verifier *signatureVerifier
	if keys != nil {
		verifier = newSignatureVerifier(keys)
	}
	position := 0
	_, _, err := engine.ScanEventLog(engine.MainEventLogFile(), 0, func(stored core.StoredEvent) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		event := stored.Event
		if event.IsSystemEvent() {
			return nil
		}
		position++
		report.CheckedEvents++
		subjects[event.Subject] = true
		if report.Problem == "" {
			switch {
			case upgraded && event.HashVersion != core.CurrentHashVersion:
				report.Problem = fmt.Sprintf("Event %s downgrades the hash algorithm", event.ID)
			case !core.HasValidHash(event):
				report.Problem = fmt.Sprintf("Event %s has an invalid version %d hash", event.ID, event.HashVersion)
			case event.PredecessorHash != previous:
				report.Problem = fmt.Sprintf("Event %s has an incorrect predecessor hash", event.ID)
			}
		}
		if event.HashVersion == core.CurrentHashVersion {
			upgraded = true
		} else {
			report.LegacyEvents++
		}
		if event.Signature == "" {
			report.UnsignedEvents++
		} else if verifier != nil {
			verifier.add(position, event)
		}
		previous = event.Hash
		return nil
	})
	if verifier != nil {
		firstInvalid, invalidID := verifier.finish()
		report.SignedEvents = verifier.validCount
		report.UnknownKeyEvents = verifier.unknownKeyCount
		if err == nil && report.Problem == "" && firstInvalid >= 0 {
			report.Problem = fmt.Sprintf("Event %s has an invalid signature", invalidID)
		}
	}
	if err != nil {
		return nil, err
	}
	for subject := range subjects {
		report.Subjects = append(report.Subjects, subject)
	}
	sort.Strings(report.Subjects)
	return report, nil
}
