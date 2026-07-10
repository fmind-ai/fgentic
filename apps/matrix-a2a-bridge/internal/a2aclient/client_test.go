package a2aclient

import (
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// The result mapping is the one genuinely fiddly piece of glue (Task vs Message sum type,
// terminal vs still-running tasks). These tests pin its behaviour without a live agent.
func TestToResult_Message(t *testing.T) {
	msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("hello from agent"))
	msg.ContextID = "ctx-0"
	res := toResult(msg)
	if res.Text != "hello from agent" {
		t.Errorf("Text = %q, want %q", res.Text, "hello from agent")
	}
	if !res.Terminal {
		t.Error("a bare Message must be terminal")
	}
	if res.ContextID != "ctx-0" {
		t.Errorf("ContextID = %q, want ctx-0", res.ContextID)
	}
}

func TestToResult_TaskArtifact(t *testing.T) {
	task := &a2a.Task{
		ID:        "task-1",
		ContextID: "ctx-1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{
			{Parts: a2a.ContentParts{a2a.NewTextPart("the pod is OOMKilled")}},
		},
	}
	res := toResult(task)
	if res.Text != "the pod is OOMKilled" {
		t.Errorf("Text = %q", res.Text)
	}
	if !res.Terminal {
		t.Error("a completed Task must be terminal")
	}
	if res.ContextID != "ctx-1" || res.TaskID != "task-1" {
		t.Errorf("ContextID/TaskID = %q/%q", res.ContextID, res.TaskID)
	}
}

func TestToResult_TaskStatusMessageFallback(t *testing.T) {
	task := &a2a.Task{
		ContextID: "ctx-2",
		Status: a2a.TaskStatus{
			State:   a2a.TaskStateCompleted,
			Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("done")),
		},
	}
	if res := toResult(task); res.Text != "done" {
		t.Errorf("Text = %q, want done", res.Text)
	}
}

func TestToResult_WorkingTaskIsNotTerminal(t *testing.T) {
	task := &a2a.Task{
		ID:        "task-3",
		ContextID: "ctx-3",
		Status: a2a.TaskStatus{
			State:   a2a.TaskStateWorking,
			Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("crunching…")),
		},
	}
	res := toResult(task)
	if res.Terminal {
		t.Error("a working Task must not be terminal")
	}
	if res.TaskID != "task-3" {
		t.Errorf("TaskID = %q, want task-3", res.TaskID)
	}
	if res.Text != "crunching…" {
		t.Errorf("interim Text = %q, want the status message", res.Text)
	}
}

func TestToResult_EmptyTerminalTaskGetsPlaceholder(t *testing.T) {
	task := &a2a.Task{Status: a2a.TaskStatus{State: a2a.TaskStateFailed}}
	if res := toResult(task); res.Text == "" {
		t.Error("terminal Task with no output should yield a placeholder, got empty string")
	}
}
