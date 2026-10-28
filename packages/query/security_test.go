package query

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

func TestQuotedKeywordsAndOperators(t *testing.T) {
	for _, literal := range []string{"A AND B", "A OR B", "hello LIMIT 1 world", "WHERE MAP TOP 2", "text IN value", "a != b", "a,b"} {
		source := &sliceSource{events: []*core.Event{{Subject: "/a/1", Type: "t"}}}
		source.events[0].Data, _ = json.Marshal(map[string]string{"text": literal})
		result, err := Run(context.Background(), source, "STREAM e FROM events WHERE e.data.text == '"+literal+"' MAP COUNT()")
		if err != nil || result != 1 {
			t.Fatalf("%q: %v %v", literal, result, err)
		}
	}
}

func TestBooleanPrecedenceAndBetween(t *testing.T) {
	for text, want := range map[string]int{
		"e.type == 'a' OR e.type == 'b' AND e.data.amount == 20": 2,
		"e.data.amount BETWEEN 10 AND 30 AND e.type != 'b'":      2,
	} {
		got, err := Run(context.Background(), newSource(), "STREAM e FROM events WHERE "+text+" MAP COUNT()")
		if err != nil || got != want {
			t.Fatalf("%s: %v %v", text, got, err)
		}
	}
}

func TestParseRejectsInvalidAndOversizedQueries(t *testing.T) {
	for _, text := range []string{
		"STREAM e FROM other", "STREAM e FROM events WHERE nonsense",
		"STREAM e FROM events WHERE e.type == 'unterminated",
		"STREAM e FROM events LIMIT 999999999999999999999999999999999",
		strings.Repeat("x", 65537),
	} {
		if _, err := Parse(text); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, newSource(), "STREAM e FROM events"); err == nil {
		t.Fatal("cancellation ignored")
	}
}

func FuzzParseAndExecute(f *testing.F) {
	for _, seed := range []string{"STREAM e FROM events", "STREAM e FROM events WHERE e.type == 'A AND B'", "FROM x IN (STREAM e FROM events MAP {*}) MAP {*}", "STREAM e FROM events WHERE e.data.amount BETWEEN 1 AND 2 MAP COUNT()", "STREAM e FROM events MAP COUNT() garbage", "LIMIT 5 FROM e IN events PROJECT INTO { a: e.data.a }"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 65536 {
			return
		}
		parsed, err := Parse(text)
		if err != nil {
			return
		}
		_, _ = Execute(context.Background(), newSource(), parsed)
	})
}

func BenchmarkQueryIndexedFilter(b *testing.B) {
	source := newSource()
	parsed, _ := Parse("STREAM e FROM events WHERE e.type == 'a' MAP COUNT()")
	b.ReportAllocs()
	for b.Loop() {
		source.selectors = nil
		if _, err := Execute(context.Background(), source, parsed); err != nil {
			b.Fatal(err)
		}
	}
}
