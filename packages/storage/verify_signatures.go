package storage

import (
	"runtime"
	"sync"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// signatureCheckBatchSize is the number of signed events one worker checks
// at a time. Ed25519 verification costs about 45 microseconds per event, so
// the audit spreads it over every core while the scan continues.
const signatureCheckBatchSize = 1024

// signatureCheck is one signed event, by position in the log.
type signatureCheck struct {
	position int
	event    *core.Event
}

// signatureVerifier checks signatures concurrently with the log scan and
// reports the first failure in log order.
type signatureVerifier struct {
	keys    core.SigningKeySet
	batches chan []signatureCheck
	pending []signatureCheck
	group   sync.WaitGroup

	mutex           sync.Mutex
	firstInvalid    int
	firstInvalidID  string
	unknownKeyCount int
	validCount      int
}

func newSignatureVerifier(keys core.SigningKeySet) *signatureVerifier {
	verifier := &signatureVerifier{keys: keys, batches: make(chan []signatureCheck, runtime.GOMAXPROCS(0)), firstInvalid: -1}
	for range runtime.GOMAXPROCS(0) {
		verifier.group.Go(func() {
			for batch := range verifier.batches {
				verifier.check(batch)
			}
		})
	}
	return verifier
}

func (verifier *signatureVerifier) add(position int, event *core.Event) {
	verifier.pending = append(verifier.pending, signatureCheck{position: position, event: event})
	if len(verifier.pending) >= signatureCheckBatchSize {
		verifier.batches <- verifier.pending
		verifier.pending = nil
	}
}

func (verifier *signatureVerifier) check(batch []signatureCheck) {
	valid, unknownKey := 0, 0
	firstInvalid, firstInvalidID := -1, ""
	for _, item := range batch {
		switch verifier.keys.VerifySignature(item.event) {
		case core.SignatureValid:
			valid++
		case core.SignatureUnknownKey:
			unknownKey++
		case core.SignatureInvalid:
			if firstInvalid < 0 || item.position < firstInvalid {
				firstInvalid, firstInvalidID = item.position, item.event.ID
			}
		}
	}
	verifier.mutex.Lock()
	defer verifier.mutex.Unlock()
	verifier.validCount += valid
	verifier.unknownKeyCount += unknownKey
	if firstInvalid >= 0 && (verifier.firstInvalid < 0 || firstInvalid < verifier.firstInvalid) {
		verifier.firstInvalid, verifier.firstInvalidID = firstInvalid, firstInvalidID
	}
}

// finish waits for every check. It returns the position and ID of the first
// invalid signature in log order, or -1.
func (verifier *signatureVerifier) finish() (int, string) {
	if len(verifier.pending) > 0 {
		verifier.batches <- verifier.pending
		verifier.pending = nil
	}
	close(verifier.batches)
	verifier.group.Wait()
	return verifier.firstInvalid, verifier.firstInvalidID
}
