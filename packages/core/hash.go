package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// CurrentHashVersion binds metadata, payload, algorithm and predecessor.
const CurrentHashVersion = 3

// Hash algorithms of the event hash chain. New events always use HashEvent.
// The two older algorithms are kept forever, because events committed by
// early releases still carry hashes produced by them.

// HashEvent returns the SHA-256 hash of an event as hex string. It covers
// source, subject, type, specversion, id, time, datacontenttype, data and
// the reference data key. The predecessor hash is linked separately.
func HashEvent(event *Event) string {
	if event.HashVersion == CurrentHashVersion {
		legacy := *event
		legacy.HashVersion = 0
		return sha256JSON(struct {
			Version     int    `json:"version"`
			Predecessor string `json:"predecessor"`
			EventHash   string `json:"eventHash"`
		}{CurrentHashVersion, event.PredecessorHash, HashEvent(&legacy)})
	}
	return sha256JSON(struct {
		Source          string          `json:"source"`
		Subject         string          `json:"subject"`
		Type            string          `json:"type"`
		SpecVersion     string          `json:"specversion"`
		ID              string          `json:"id"`
		Time            string          `json:"time"`
		DataContentType string          `json:"datacontenttype"`
		Data            json.RawMessage `json:"data"`
		Ref             string          `json:"ref,omitempty"`
	}{
		Source:          event.Source,
		Subject:         event.Subject,
		Type:            event.Type,
		SpecVersion:     event.SpecVersion,
		ID:              event.ID,
		Time:            event.Time.UTC().Format(time.RFC3339Nano),
		DataContentType: event.DataContentType,
		Data:            event.Data,
		Ref:             event.ReferenceDataKey,
	})
}

// HashEventV1 returns the hash of the first event hash algorithm, which did
// not cover source, specversion, id and datacontenttype.
func HashEventV1(event *Event) string {
	return sha256JSON(struct {
		Subject string          `json:"subject"`
		Type    string          `json:"type"`
		Time    string          `json:"time"`
		Data    json.RawMessage `json:"data"`
		Ref     string          `json:"ref,omitempty"`
	}{
		Subject: event.Subject,
		Type:    event.Type,
		Time:    event.Time.UTC().Format(time.RFC3339Nano),
		Data:    event.Data,
		Ref:     event.ReferenceDataKey,
	})
}

// HashData returns the hash of the oldest algorithm, which covered the
// normalized event data only.
func HashData(data json.RawMessage) string {
	if len(data) == 0 {
		return sha256Hex([]byte("{}"))
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return sha256Hex(data)
	}
	normalized, err := json.Marshal(object)
	if err != nil {
		return sha256Hex(data)
	}
	return sha256Hex(normalized)
}

// HasValidHash reports whether the event hash matches one of the three hash
// algorithms GenesisDB ever used.
func HasValidHash(event *Event) bool {
	if event == nil {
		return false
	}
	switch event.HashVersion {
	case CurrentHashVersion:
		return event.Hash == HashEvent(event)
	case 0:
		return event.Hash == HashEvent(event) || event.Hash == HashEventV1(event) || event.Hash == HashData(event.Data)
	default:
		return false
	}
}

func sha256JSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		// Only invalid json.RawMessage data can fail here. Such data never
		// reaches a hash because events are validated before they are hashed.
		return ""
	}
	return sha256Hex(encoded)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
