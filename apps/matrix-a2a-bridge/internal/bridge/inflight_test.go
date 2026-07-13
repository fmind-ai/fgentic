package bridge

import (
	"testing"

	"maunium.net/go/mautrix/id"
)

func TestInflightRegistryLifecycle(t *testing.T) {
	reg := newInflightRegistry()
	task := &inflightTask{
		placeholder:    "$ph",
		originalSender: id.NewUserID("alice", ownServer),
		cancelPoll:     func() {},
	}
	if _, ok := reg.lookup("$ph"); ok {
		t.Fatal("lookup found an unregistered task")
	}
	reg.register(task)
	got, ok := reg.lookup("$ph")
	if !ok || got != task {
		t.Fatalf("lookup = (%v, %v), want the registered task", got, ok)
	}
	reg.unregister("$ph")
	if _, ok := reg.lookup("$ph"); ok {
		t.Fatal("lookup found a task after unregister")
	}
}

func TestInflightTaskCancelIsExactlyOnce(t *testing.T) {
	canceled := 0
	task := &inflightTask{cancelPoll: func() { canceled++ }}
	alice := id.NewUserID("alice", ownServer)
	bob := id.NewUserID("bob", ownServer)

	if !task.requestCancel(alice) {
		t.Fatal("first requestCancel = false, want true (it triggered cancellation)")
	}
	if got := task.canceler(); got != alice {
		t.Fatalf("canceler = %q, want %q", got, alice)
	}
	if task.requestCancel(bob) {
		t.Fatal("second requestCancel = true, want false (already canceled)")
	}
	if got := task.canceler(); got != alice {
		t.Fatalf("canceler after duplicate = %q, want the first canceler %q", got, alice)
	}
	if canceled != 1 {
		t.Fatalf("cancelPoll invoked %d times, want exactly 1", canceled)
	}
}

func TestTaskCancelerNilSafe(t *testing.T) {
	if got := taskCanceler(nil); got != "" {
		t.Fatalf("taskCanceler(nil) = %q, want empty", got)
	}
	running := &inflightTask{cancelPoll: func() {}}
	if got := taskCanceler(running); got != "" {
		t.Fatalf("taskCanceler(running) = %q, want empty while running", got)
	}
}
