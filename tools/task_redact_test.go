package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
)

// straddleSecret is a credential made of mixed-case runs that appear nowhere
// else in a tool response, so any 4-byte window of it showing up is a leak.
const straddleSecret = "Qz8JxWv7KpLm9RtYh6NbFgDcSa"

func assertNoSecretWindow(t *testing.T, where, out string) {
	t.Helper()
	for i := 0; i+4 <= len(straddleSecret); i++ {
		if w := straddleSecret[i : i+4]; strings.Contains(out, w) {
			t.Errorf("%s leaks part of the secret (%q):\n%.600s", where, w, out[max(0, strings.Index(out, w)-200):])
			return
		}
	}
}

// A job result is redacted before it is cut to MaxJobResultBytes: a secret
// whose value straddles the cut must not survive as an unclosed quoted
// fragment in the preview.
func TestTaskResultSecretStraddlingCutIsRedacted(t *testing.T) {
	// Marshalled: {"a_pad":"xxx…","bindpw":"<secret>"}. The secret starts
	// 22+pad bytes in, so the 4096-byte cut lands inside it.
	pad := tasks.MaxJobResultBytes - 22 - len(straddleSecret)/2
	row := jobRow(21, "directoryservices.update", "SUCCESS", 1_700_000_000_000)
	row["result"] = map[string]interface{}{"a_pad": strings.Repeat("x", pad), "bindpw": straddleSecret}

	r, mgr, f := newJobFakeRegistry(t, tasks.PollerConfig{})
	f.set(func(f *jobFake) { f.jobs = []interface{}{row} })
	task, err := mgr.CreateJobTask("configure_directory_service", nil, 21, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mgr.PollNow()
	out, err := r.CallTool(t.Context(), "tasks_get", map[string]interface{}{"task_id": task.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "completed") {
		t.Fatalf("task not completed: %s", out)
	}
	assertNoSecretWindow(t, "tasks_get", out)

	out, err = r.CallTool(t.Context(), "query_jobs", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"result_truncated": true`) && !strings.Contains(out, `"error_truncated": true`) {
		t.Fatalf("query_jobs did not return the cut job: %.600s", out)
	}
	assertNoSecretWindow(t, "query_jobs", out)
}

// Failed-job error text is redacted before it is cut to MaxJobTextBytes.
func TestTaskErrorSecretStraddlingCutIsRedacted(t *testing.T) {
	// The secret starts at 1985, so the 2000-byte cut lands inside it.
	prefix := strings.Repeat("e", tasks.MaxJobTextBytes-15-len("{'bindpw': '")) + "{'bindpw': '"
	row := jobRow(22, "directoryservices.update", "FAILED", 1_700_000_000_000)
	row["error"] = prefix + straddleSecret + "'}"
	row["exception"] = prefix + straddleSecret + "'}"

	r, mgr, f := newJobFakeRegistry(t, tasks.PollerConfig{})
	f.set(func(f *jobFake) { f.jobs = []interface{}{row} })
	task, err := mgr.CreateJobTask("configure_directory_service", nil, 22, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mgr.PollNow()
	out, err := r.CallTool(t.Context(), "tasks_get", map[string]interface{}{"task_id": task.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "failed") {
		t.Fatalf("task not failed: %s", out)
	}
	assertNoSecretWindow(t, "tasks_get", out)

	out, err = r.CallTool(t.Context(), "query_jobs", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"result_truncated": true`) && !strings.Contains(out, `"error_truncated": true`) {
		t.Fatalf("query_jobs did not return the cut job: %.600s", out)
	}
	assertNoSecretWindow(t, "query_jobs", out)
}
