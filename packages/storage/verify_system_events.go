package storage

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// verifySystemEventSignatures checks the signatures of the system event log,
// the audit trail of erasures. Each signature covers the hash of its system
// event, so the hash must match the record and its predecessor in the system
// chain before the signature proves anything.
//
// Revocation points name events of the main log and do not order system
// events, so a key that is revoked at all, entirely or after a point, is
// not trusted for any system event. Its signatures count as revoked.
func (engine *Engine) verifySystemEventSignatures(ctx context.Context, trust *core.SignatureTrust, report *HashChainReport) error {
	counts := &report.SystemSignatures
	previous := core.ZeroHash
	_, _, err := engine.ScanEventLog(engine.format.SystemEventLogFile(), 0, func(stored core.StoredEvent) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		event := stored.Event
		isLinked := event.PredecessorHash == previous
		previous = event.Hash
		if event.Signature == "" {
			counts.Unsigned++
			return nil
		}
		keyID, _ := core.SignatureKeyID(event.Signature)
		if trust.Revocations.IsRevoked(keyID) {
			counts.Revoked++
			return nil
		}
		status := trust.Keys.VerifySignature(event)
		if status != core.SignatureInvalid && (!isLinked || !core.HasValidHash(event)) {
			status = core.SignatureInvalid
		}
		switch status {
		case core.SignatureValid:
			counts.Signed++
		case core.SignatureUnknownKey:
			counts.UnknownKey++
		case core.SignatureInvalid:
			if report.Problem == "" {
				report.Problem = fmt.Sprintf("System event %s has an invalid signature", event.ID)
			}
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
