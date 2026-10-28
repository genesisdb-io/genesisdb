package storage

import (
	"context"
	"fmt"
	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"sort"
)

// HashChainReport describes integrity checks, not proof against an attacker
// who can rewrite the data directory. Persist checkpoints externally for that.
type HashChainReport struct {
	CheckedEvents int
	LegacyEvents  int
	Subjects      []string
	Problem       string
}

func (report *HashChainReport) IsValid() bool { return report.Problem == "" }

func (engine *Engine) VerifyHashChain() (*HashChainReport, error) {
	return engine.VerifyHashChainContext(context.Background())
}

// VerifyHashChainContext streams raw records in physical chain order. Clock
// corrections do not change chain order. Tagged records never use fallback
// algorithms and an upgraded chain cannot downgrade to an untagged record.
func (engine *Engine) VerifyHashChainContext(ctx context.Context) (*HashChainReport, error) {
	report := &HashChainReport{}
	subjects := map[string]bool{}
	previous := core.ZeroHash
	upgraded := false
	_, _, err := engine.ScanEventLog(engine.MainEventLogFile(), 0, func(stored core.StoredEvent) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		event := stored.Event
		if event.IsSystemEvent() {
			return nil
		}
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
		previous = event.Hash
		return nil
	})
	if err != nil {
		return nil, err
	}
	for subject := range subjects {
		report.Subjects = append(report.Subjects, subject)
	}
	sort.Strings(report.Subjects)
	return report, nil
}
