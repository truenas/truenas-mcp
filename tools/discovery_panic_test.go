package tools

import (
	"bytes"
	"log"
	"os"
	"testing"
)

// ARC graph discovery runs its two lookups in goroutines, which CallTool's
// recover does not cover. A panic in either (a nil client panics in both)
// must be logged and reported as incomplete discovery (the fallback graph list is used), not crash.
func TestDiscoverArcGraphsSurvivesPanics(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	_, complete := discoverArcGraphs(nil)
	if complete {
		t.Error("discovery reported complete although both lookups panicked")
	}
	if n := bytes.Count(logs.Bytes(), []byte("ARC graph discovery")); n != 2 {
		t.Errorf("logged %d panics; want 2: %s", n, logs.String())
	}
}
