package query

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// referenceSort is the straightforward ORDER BY: resolve the field on every
// comparison. sortEvents must produce exactly this order while resolving
// each field once.
func referenceSort(events []*core.Event, ordering *Ordering, alias string) {
	sort.SliceStable(events, func(i, j int) bool {
		first := resolveFieldPath(events[i], ordering.Field, alias)
		second := resolveFieldPath(events[j], ordering.Field, alias)
		if first == nil && second == nil {
			return false
		}
		if first == nil {
			return ordering.IsDescending
		}
		if second == nil {
			return !ordering.IsDescending
		}
		if ordering.IsDescending {
			return compareValues(first, second) > 0
		}
		return compareValues(first, second) < 0
	})
}

// TestSortEvents_MatchesReferenceOrder sorts mixed payloads (integers,
// decimals, huge integers, strings, booleans, nested fields, missing fields,
// duplicates) by several fields in both directions and compares the result
// with the reference implementation event by event.
func TestSortEvents_MatchesReferenceOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var events []*core.Event
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2000; i++ {
		var payload string
		switch rng.Intn(8) {
		case 0:
			payload = fmt.Sprintf(`{"amount":%d,"nested":{"v":%d}}`, rng.Intn(100), rng.Intn(10))
		case 1:
			payload = fmt.Sprintf(`{"amount":%d.%02d}`, rng.Intn(100), rng.Intn(100))
		case 2:
			payload = fmt.Sprintf(`{"amount":9007199254740%03d}`, rng.Intn(1000))
		case 3:
			payload = fmt.Sprintf(`{"amount":"%c%d"}`, 'a'+rune(rng.Intn(5)), rng.Intn(10))
		case 4:
			payload = fmt.Sprintf(`{"amount":%t}`, rng.Intn(2) == 0)
		case 5:
			payload = `{"other":1}`
		case 6:
			payload = fmt.Sprintf(`{"amount":-%d}`, rng.Intn(100))
		default:
			payload = `{"amount":null}`
		}
		events = append(events, &core.Event{
			ID:      fmt.Sprintf("%04d", i),
			Subject: fmt.Sprintf("/s/%d", rng.Intn(50)),
			Type:    fmt.Sprintf("t%d", rng.Intn(3)),
			Time:    base.Add(time.Duration(rng.Intn(1000)) * time.Second),
			Data:    json.RawMessage(payload),
		})
	}
	for _, field := range []string{"e.data.amount", "e.data.nested.v", "e.time", "e.subject", "e.type", "e.id", "e.data.missing"} {
		for _, descending := range []bool{false, true} {
			ordering := &Ordering{Field: field, IsDescending: descending}
			expected := append([]*core.Event(nil), events...)
			referenceSort(expected, ordering, "e")
			actual := append([]*core.Event(nil), events...)
			sortEvents(actual, ordering, "e")
			for position := range expected {
				if expected[position].ID != actual[position].ID {
					t.Fatalf("%s desc=%v: position %d differs: want %s, got %s", field, descending, position, expected[position].ID, actual[position].ID)
				}
			}
		}
	}
}

// BenchmarkSortEvents_DataField measures ORDER BY on a payload field, the
// case where resolving the field means decoding JSON.
func BenchmarkSortEvents_DataField(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	var events []*core.Event
	for i := 0; i < 20000; i++ {
		events = append(events, &core.Event{ID: fmt.Sprint(i), Data: json.RawMessage(fmt.Sprintf(`{"amount":%d,"category":"%c","note":"%s"}`, rng.Intn(1000), 'a'+rune(rng.Intn(3)), "x"))})
	}
	ordering := &Ordering{Field: "e.data.amount", IsDescending: true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copied := append([]*core.Event(nil), events...)
		sortEvents(copied, ordering, "e")
	}
}
