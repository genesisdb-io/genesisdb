package httpserver

import (
	"strings"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/query"
)

// Every example of the agent reference must be valid GDBQL, so a syntax
// change cannot leave agents with broken examples.
func TestMCPGDBQLReferenceExamplesParse(t *testing.T) {
	_, block, found := strings.Cut(gdbqlReference, "## Examples\n\n```\n")
	if !found {
		t.Fatal("examples block missing")
	}
	block, _, found = strings.Cut(block, "```")
	if !found {
		t.Fatal("examples block not closed")
	}
	examples := 0
	for _, line := range strings.Split(strings.TrimSpace(block), "\n") {
		parsed, err := query.Parse(line)
		if err != nil {
			t.Errorf("example does not parse: %s: %v", line, err)
			continue
		}
		// The parser tolerates unknown trailing words, so also require
		// the projection every example ends with.
		if !parsed.Projection.IsFullEvent && len(parsed.Projection.Fields) == 0 {
			t.Errorf("example has no projection: %s", line)
		}
		examples++
	}
	if examples < 5 {
		t.Fatalf("only %d examples", examples)
	}
}
