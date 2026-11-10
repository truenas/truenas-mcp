package tasks

import "testing"

func TestWithExpectedMethodUsedForIdentity(t *testing.T) {
	task := &Task{ToolName: "start_app"}
	if !task.methodMatches("app.start") {
		t.Fatal("start_app should match app.start from the tool table")
	}
	WithExpectedMethod("custom.job")(task)
	if task.methodMatches("app.start") {
		t.Fatal("WithExpectedMethod should override the tool table")
	}
	if !task.methodMatches("custom.job") {
		t.Fatal("WithExpectedMethod should accept the planned method")
	}
}
