package main

import (
	"strings"
	"testing"
)

// TestNitsCIBuildRunsFullStrengthRedactTests is the #151 follow-up: after
// the race-and-coverage fold, build CI must still run the redaction
// package once without the race detector so the randomized and linear
// tests run at full strength.
func TestNitsCIBuildRunsFullStrengthRedactTests(t *testing.T) {
	src := issue114Read(t, ".github/workflows/build.yml")
	found := false
	for _, line := range testInvocations(src) {
		if strings.Contains(line, "go test") &&
			strings.Contains(line, "-count=1") &&
			strings.Contains(line, "./redact") &&
			!strings.Contains(line, "-race") {
			found = true
			continue
		}
	}
	if !found {
		t.Errorf("build.yml has no non-race `go test -count=1 ./redact` step; invocations = %q", testInvocations(src))
	}
}

// TestNitsCIMakefileRaceVariable lets contributors run `make test RACE=`
// for a faster pass, while the default remains -race.
func TestNitsCIMakefileRaceVariable(t *testing.T) {
	makefile := issue114Read(t, "Makefile")
	if !strings.Contains(makefile, "RACE ?= -race") {
		t.Error("Makefile should define `RACE ?= -race` so `make test RACE=` disables the race detector")
	}
	recipe := makefileTestRecipe(t, makefile)
	if !strings.Contains(recipe, "$(RACE)") {
		t.Errorf("Makefile test target = %q, want go test with $(RACE)", recipe)
	}
}

// TestNitsCIErrcheckExcludesAreExact drops unused tls/net/short-path Conn
// entries, and the poller comment must say a refused write is not retried.
func TestNitsCIErrcheckExcludesAreExact(t *testing.T) {
	src := issue114Read(t, ".errcheck-excludes")
	for _, dead := range []string{
		"(*tls.Conn).Close",
		"(*net.TCPListener).Close",
		"(*net.TCPConn).Close",
		"(*websocket.Conn).Close",
		"(*websocket.Conn).SetReadDeadline",
	} {
		if strings.Contains(src, dead) {
			t.Errorf(".errcheck-excludes still has unused entry %s", dead)
		}
	}
	// The qualified websocket entries must remain.
	for _, keep := range []string{
		"(*github.com/gorilla/websocket.Conn).Close",
		"(*github.com/gorilla/websocket.Conn).SetReadDeadline",
	} {
		if !strings.Contains(src, keep) {
			t.Errorf(".errcheck-excludes dropped live entry %s", keep)
		}
	}
	lower := strings.ToLower(src)
	if !strings.Contains(lower, "gone") || !strings.Contains(lower, "finished") {
		t.Error("poller exclusion comment should say a refused write means the task is gone or already finished")
	}
	if strings.Contains(lower, "next tick retries") {
		t.Error("poller exclusion comment still says the next tick retries")
	}
}
