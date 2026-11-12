package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	mathrand "math/rand"
	"net/http"
	"strings"
)

// crashRecord is one commit a client sent, with everything the client was
// told about it.
type crashRecord struct {
	round, writer int
	markers       []string
	subjects      []string
	// ids holds the event IDs of the response once the commit is
	// acknowledged.
	ids            []string
	isAcknowledged bool
	// isRejected marks a commit answered with 412.
	isRejected bool
	// raceSubject is the subject two clients try to create first.
	raceSubject string
	// erasableSubject is the subject whose reference payload (the last
	// event of the commit) may be erased later.
	erasableSubject string
}

type erasureState int

const (
	erasureRequested erasureState = iota + 1
	erasureAcknowledged
)

type crashEvent struct {
	Source  string         `json:"source"`
	Subject string         `json:"subject"`
	Type    string         `json:"type"`
	Data    map[string]any `json:"data"`
	Options map[string]any `json:"options,omitempty"`
}

type crashPrecondition struct {
	Type    string            `json:"type"`
	Payload map[string]string `json:"payload"`
}

type storedCrashEvent struct {
	ID        string          `json:"id"`
	Subject   string          `json:"subject"`
	Data      json.RawMessage `json:"data"`
	Signature string          `json:"signature"`
}

// newCommit builds a random commit: plain events, a race of two clients for
// one new subject (by subject or by query precondition), an erasable
// reference payload, or a commit that requires an acknowledged subject.
func (harness *crashHarness) newCommit(random *mathrand.Rand, round, writer, sequence int) (*crashRecord, string) {
	record := &crashRecord{round: round, writer: writer}
	tag := fmt.Sprintf("r%d-w%d-c%d", round, writer, sequence)
	var events []crashEvent
	var preconditions []crashPrecondition
	add := func(subject string, isReference bool) {
		marker := fmt.Sprintf("%s-e%d", tag, len(events))
		event := crashEvent{Source: "crash-test", Subject: subject, Type: fmt.Sprintf("type-%d", random.Intn(5)), Data: map[string]any{"m": marker, "pad": strings.Repeat("x", random.Intn(400))}}
		if isReference {
			event.Options = map[string]any{"storeDataAsReference": true}
		}
		events = append(events, event)
		record.markers = append(record.markers, marker)
		record.subjects = append(record.subjects, subject)
	}
	switch random.Intn(8) {
	case 0:
		record.raceSubject = fmt.Sprintf("/race/r%d/p%d/%d", round, writer/2, sequence)
		add(record.raceSubject, false)
		preconditions = append(preconditions, crashPrecondition{Type: "isSubjectNew", Payload: map[string]string{"subject": record.raceSubject}})
	case 1:
		record.raceSubject = fmt.Sprintf("/query-race/r%d/p%d/%d", round, writer/2, sequence)
		add(record.raceSubject, false)
		preconditions = append(preconditions, crashPrecondition{Type: "isQueryResultTrue", Payload: map[string]string{"query": fmt.Sprintf("STREAM e FROM events WHERE e.subject == '%s' MAP COUNT() == 0", record.raceSubject)}})
	case 2:
		record.erasableSubject = fmt.Sprintf("/erase/r%d/w%d/%d", round, writer, sequence)
		add(record.erasableSubject+"/meta", false)
		add(record.erasableSubject+"/profile", true)
	case 3:
		harness.mutex.Lock()
		known := harness.subjects[writer]
		harness.mutex.Unlock()
		if len(known) > 0 {
			existing := known[random.Intn(len(known))]
			preconditions = append(preconditions, crashPrecondition{Type: "isSubjectExisting", Payload: map[string]string{"subject": existing}})
		}
		add(fmt.Sprintf("/load/r%d/w%d/%d/0", round, writer, sequence), false)
	default:
		for position := 0; position <= random.Intn(3); position++ {
			add(fmt.Sprintf("/load/r%d/w%d/%d/%d", round, writer, sequence, position), random.Intn(3) == 0)
		}
	}
	body, _ := json.Marshal(map[string]any{"events": events, "preconditions": preconditions})
	harness.mutex.Lock()
	harness.records = append(harness.records, record)
	harness.mutex.Unlock()
	return record, string(body)
}

// runWriter commits until the round ends or the server stops answering.
func (harness *crashHarness) runWriter(ctx context.Context, round, writer int) {
	random := mathrand.New(mathrand.NewSource(int64(round)<<20 | int64(writer)))
	for sequence := 0; ctx.Err() == nil; sequence++ {
		record, body := harness.newCommit(random, round, writer, sequence)
		status, content, err := harness.call(ctx, http.MethodPost, "/api/v2/commit", body)
		if err != nil {
			return // the outcome of this commit is unknown
		}
		switch status {
		case http.StatusOK:
			var stored []storedCrashEvent
			if err := json.Unmarshal(content, &stored); err != nil || len(stored) != len(record.markers) {
				harness.t.Errorf("commit %s: unusable response: %v %.300s", record.markers[0], err, content)
				return
			}
			harness.mutex.Lock()
			for position, event := range stored {
				if event.Subject != record.subjects[position] || !bytes.Contains(event.Data, []byte(`"`+record.markers[position]+`"`)) {
					harness.t.Errorf("commit %s: the response carries a foreign event: %s %.200s", record.markers[position], event.Subject, event.Data)
				}
				if event.Signature == "" {
					harness.t.Errorf("commit %s: acknowledged without a signature", record.markers[position])
				}
				record.ids = append(record.ids, event.ID)
			}
			record.isAcknowledged = true
			known := append(harness.subjects[writer], record.subjects[0])
			if len(known) > 64 {
				known = known[len(known)-64:]
			}
			harness.subjects[writer] = known
			harness.mutex.Unlock()
		case http.StatusPreconditionFailed:
			if record.raceSubject == "" {
				harness.t.Errorf("commit %s: a subject of an acknowledged commit was reported missing: %.300s", record.markers[0], content)
				return
			}
			harness.mutex.Lock()
			record.isRejected = true
			harness.mutex.Unlock()
		case http.StatusBadRequest:
			harness.t.Errorf("commit %s rejected as invalid: %.300s", record.markers[0], content)
			return
		default:
			return // the server is going down or unhealthy, outcome unknown
		}
	}
}

// runMaintenance competes with the commits for the writer lock: audits,
// backups and erasures of acknowledged reference payloads.
func (harness *crashHarness) runMaintenance(ctx context.Context) {
	for ctx.Err() == nil {
		status, content, err := harness.call(ctx, http.MethodGet, "/api/v2/admin/audit", "")
		if err != nil || status != http.StatusOK {
			return
		}
		if !strings.HasPrefix(string(content), "Audit passed") {
			harness.t.Errorf("audit under write load failed: %.500s", content)
			return
		}
		if status, _, err = harness.call(ctx, http.MethodGet, "/api/v2/admin/backup", ""); err != nil || status != http.StatusOK {
			return
		}
		harness.mutex.Lock()
		harness.totals.maintenanceCalls += 2
		subject := ""
		for _, record := range harness.records {
			if record.isAcknowledged && record.erasableSubject != "" && harness.erasures[record.erasableSubject] == 0 {
				subject = record.erasableSubject
				harness.erasures[subject] = erasureRequested
				break
			}
		}
		harness.mutex.Unlock()
		if subject == "" {
			continue
		}
		body, _ := json.Marshal(map[string]string{"subject": subject})
		status, content, err = harness.call(ctx, http.MethodPost, "/api/v2/admin/erase", string(body))
		if err != nil {
			return
		}
		switch status {
		case http.StatusOK:
			harness.mutex.Lock()
			harness.erasures[subject] = erasureAcknowledged
			harness.totals.erasures++
			harness.mutex.Unlock()
		case http.StatusNotFound, http.StatusUnprocessableEntity:
			harness.t.Errorf("erase of the acknowledged subject %s: %d %.300s", subject, status, content)
			return
		default:
			return
		}
	}
}

// verify compares the running store with what the clients were told and
// returns the number of events. It reports:
//   - an acknowledged commit that is missing, changed or reordered,
//   - a commit that is only partly present, acknowledged or not,
//   - a rejected commit that was written,
//   - a subject created by more than one of the commits that raced for it,
//   - an erased payload that came back, an event nobody sent, a duplicate,
//   - a failed hash chain or signature audit.
func (harness *crashHarness) verify(round int) int {
	t := harness.t
	ctx := context.Background()
	status, content, err := harness.call(ctx, http.MethodGet, "/api/v2/admin/audit", "")
	if err != nil || status != http.StatusOK || !strings.HasPrefix(string(content), "Audit passed") {
		t.Errorf("round %d: audit after restart: %d %v %.500s", round, status, err, content)
		return 0
	}
	audit := string(content)
	status, content, err = harness.call(ctx, http.MethodPost, "/api/v2/fetch", `{"subject":"/"}`)
	if err != nil || status != http.StatusOK {
		t.Errorf("round %d: fetch of every event: %d %v %.300s", round, status, err, content)
		return 0
	}
	var all []storedCrashEvent
	if err := json.Unmarshal(content, &all); err != nil {
		t.Errorf("round %d: decode the fetch: %v", round, err)
		return 0
	}
	if len(all) > 0 && !strings.Contains(audit, ", 0 unsigned, 0 signed by an untrusted key") {
		t.Errorf("round %d: not every event carries a valid signature: %s", round, audit)
	}
	positionOfID := make(map[string]int, len(all))
	positionOfMarker := make(map[string]int, len(all))
	for position, event := range all {
		if _, isDuplicate := positionOfID[event.ID]; isDuplicate {
			t.Errorf("round %d: event ID %s is stored twice", round, event.ID)
		}
		positionOfID[event.ID] = position
		var data struct {
			Marker string `json:"m"`
		}
		json.Unmarshal(event.Data, &data)
		if data.Marker == "" {
			continue // an erased reference payload
		}
		if _, isDuplicate := positionOfMarker[data.Marker]; isDuplicate {
			t.Errorf("round %d: event %s is stored twice", round, data.Marker)
		}
		positionOfMarker[data.Marker] = position
	}

	harness.mutex.Lock()
	defer harness.mutex.Unlock()
	isExpected := make([]bool, len(all))
	creators := map[string]int{}
	lastPosition := map[[2]int]int{}
	acknowledged := 0
	for _, record := range harness.records {
		name := record.markers[0]
		if !record.isAcknowledged {
			present := 0
			for index, marker := range record.markers {
				if position, isPresent := positionOfMarker[marker]; isPresent {
					present++
					isExpected[position] = true
					if all[position].Subject != record.subjects[index] {
						t.Errorf("round %d: %s is stored under %s", round, marker, all[position].Subject)
					}
				}
			}
			if present != 0 && present != len(record.markers) {
				t.Errorf("round %d: commit %s is partly present: %d of %d events", round, name, present, len(record.markers))
			}
			if record.isRejected && present != 0 {
				t.Errorf("round %d: commit %s was rejected with 412 and is stored", round, name)
			}
			if present > 0 && record.raceSubject != "" {
				creators[record.raceSubject]++
			}
			continue
		}
		acknowledged++
		previous := -1
		for index, id := range record.ids {
			position, isPresent := positionOfID[id]
			if !isPresent {
				t.Errorf("round %d: ACKNOWLEDGED COMMIT LOST: event %s (%s) of commit %s", round, id, record.markers[index], name)
				continue
			}
			isExpected[position] = true
			event := all[position]
			if event.Subject != record.subjects[index] {
				t.Errorf("round %d: acknowledged event %s is stored under %s", round, record.markers[index], event.Subject)
			}
			hasMarker := bytes.Contains(event.Data, []byte(`"`+record.markers[index]+`"`))
			erasure := erasureState(0)
			if record.erasableSubject != "" && index == len(record.ids)-1 {
				erasure = harness.erasures[record.erasableSubject]
			}
			switch {
			case erasure == erasureAcknowledged && string(event.Data) != "{}":
				t.Errorf("round %d: the erased payload of %s came back: %.200s", round, record.markers[index], event.Data)
			case erasure == 0 && !hasMarker:
				t.Errorf("round %d: acknowledged event %s lost its payload: %.200s", round, record.markers[index], event.Data)
			}
			if previous >= 0 && position != previous+1 {
				t.Errorf("round %d: the events of commit %s are not adjacent in the log", round, name)
			}
			previous = position
		}
		if record.raceSubject != "" {
			creators[record.raceSubject]++
		}
		if first, isPresent := positionOfID[record.ids[0]]; isPresent {
			key := [2]int{record.round, record.writer}
			if earlier, hasEarlier := lastPosition[key]; hasEarlier && first < earlier {
				t.Errorf("round %d: commit %s is stored before an earlier acknowledged commit of the same client", round, name)
			}
			lastPosition[key] = first
		}
	}
	for _, record := range harness.records {
		if record.isRejected && creators[record.raceSubject] == 0 {
			t.Errorf("round %d: %s was rejected as existing, but no commit created it", round, record.raceSubject)
		}
	}
	for subject, count := range creators {
		if count > 1 {
			t.Errorf("round %d: %d commits created %s, a precondition let more than one pass", round, count, subject)
		}
	}
	for position, expected := range isExpected {
		if !expected {
			t.Errorf("round %d: the store holds an event no client sent: %s %s", round, all[position].ID, all[position].Subject)
		}
	}
	if round > 0 {
		harness.totals.verifiedEvents += len(all)
	}
	harness.totals.acknowledged = harness.totals.acknowledgedInEarlierStores + acknowledged
	return len(all)
}
