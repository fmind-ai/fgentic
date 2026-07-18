package bridge

import (
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
)

func TestParsePlaintextCommand(t *testing.T) {
	tests := map[string]struct {
		body string
		want plaintextCommand
	}{
		"ordinary text": {
			body: "please ask an agent",
			want: plaintextCommand{},
		},
		"ask": {
			body: "  /ask   k8s   inspect the pod\nwithout changing it  ",
			want: plaintextCommand{
				kind: plaintextCommandAsk, agent: "k8s", prompt: "inspect the pod\nwithout changing it",
			},
		},
		"ask alias": {
			body: "!ask k8s inspect the pod",
			want: plaintextCommand{kind: plaintextCommandAsk, agent: "k8s", prompt: "inspect the pod"},
		},
		"agents": {
			body: "/agents",
			want: plaintextCommand{kind: plaintextCommandAgents},
		},
		"agent detail": {
			body: "/agents k8s",
			want: plaintextCommand{kind: plaintextCommandAgents, query: "k8s"},
		},
		"budget": {
			body: "/budget",
			want: plaintextCommand{kind: plaintextCommandBudget},
		},
		"budget alias": {
			body: "!budget",
			want: plaintextCommand{kind: plaintextCommandBudget},
		},
		"forget": {
			body: "/forget @agent-k8s:fgentic.fmind.ai",
			want: plaintextCommand{kind: plaintextCommandForget, agent: "@agent-k8s:fgentic.fmind.ai"},
		},
		"forget alias": {
			body: "!forget k8s",
			want: plaintextCommand{kind: plaintextCommandForget, agent: "k8s"},
		},
		"missing forget agent": {
			body: "!forget",
			want: plaintextCommand{kind: plaintextCommandInvalid},
		},
		"missing ask prompt": {
			body: "/ask k8s",
			want: plaintextCommand{kind: plaintextCommandInvalid},
		},
		"extra agents argument": {
			body: "/agents k8s extra",
			want: plaintextCommand{kind: plaintextCommandInvalid},
		},
		"extra budget argument": {
			body: "/budget now",
			want: plaintextCommand{kind: plaintextCommandInvalid},
		},
		"unknown": {
			body: "/delegate k8s prompt",
			want: plaintextCommand{kind: plaintextCommandInvalid},
		},
		"unrelated bang command": {
			body: "!deploy k8s",
			want: plaintextCommand{},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := parsePlaintextCommand(tt.body); got != tt.want {
				t.Fatalf("parsePlaintextCommand(%q) = %+v, want %+v", tt.body, got, tt.want)
			}
		})
	}
}

func TestAskCommandUsesExistingDelegationAndRateLimitPath(t *testing.T) {
	t.Run("delegates", func(t *testing.T) {
		client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "done", Terminal: true}}
		b, _, evt, _, recorder := pollingHarness(t, client)
		evt.ID = "$ask"
		evt.Content.Parsed = &event.MessageEventContent{
			MsgType: event.MsgText,
			Body:    "/ask @agent-k8s:" + ownServer + " inspect the pod",
		}

		b.HandleMessage(t.Context(), evt)
		b.dispatcher.Wait()
		if client.callCount != 1 || !strings.Contains(client.callText, "inspect the pod") ||
			strings.Contains(client.callText, "/ask") {
			t.Fatalf("A2A calls = %d, prompt = %q", client.callCount, client.callText)
		}
		events := recorder.snapshot()
		if len(events) != 1 || events[0].Body != "done" {
			t.Fatalf("command replies = %#v", events)
		}
	})

	t.Run("rate limited", func(t *testing.T) {
		client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "must not run", Terminal: true}}
		b, _, evt, _, recorder := pollingHarness(t, client)
		b.senderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
		sender := b.agents.IdentifySender(evt.Sender)
		if !b.senderLimits.Allow(sender.rateLimitKey("agent-k8s")) {
			t.Fatal("failed to consume sender limiter fixture token")
		}
		evt.ID = "$ask-rate-limited"
		evt.Content.Parsed = &event.MessageEventContent{MsgType: event.MsgText, Body: "/ask k8s inspect the pod"}

		b.HandleMessage(t.Context(), evt)
		b.dispatcher.Wait()
		if client.callCount != 0 {
			t.Fatalf("rate-limited /ask made %d A2A calls", client.callCount)
		}
		events := recorder.snapshot()
		if len(events) != 1 || events[0].Body != failureMessage(errorRateLimit, "", 0) {
			t.Fatalf("rate-limited /ask replies = %#v", events)
		}
	})
}

func TestCommandNoticesAreActionableAutomatedAndBounded(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, recorder := pollingHarness(t, client)
	prepareDirectoryBot(t, b, evt.RoomID)
	b.noticeSenderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	b.noticeRoomLimits = newLimiters(60, 10, testRateLimitBucketCapacity)

	unknownCommand := directoryEventWithBody("$unknown-command", evt.Sender, evt.RoomID, "/delegate k8s do work")
	b.HandleMessage(t.Context(), unknownCommand)
	unknownAgent := directoryEventWithBody("$unknown-agent", evt.Sender, evt.RoomID, "/ask missing do work")
	b.HandleMessage(t.Context(), unknownAgent)

	events := recorder.snapshot()
	if len(events) != 1 || events[0].Body != commandHelpText() {
		t.Fatalf("bounded command notices = %#v", events)
	}
	if events[0].MsgType != event.MsgNotice || events[0].RelatesTo.GetReplyTo() != unknownCommand.ID {
		t.Fatalf("command response shape = %+v", events[0])
	}
	raw := recorder.rawSnapshot(t)
	if len(raw) != 1 || raw[0][automatedMixinKey] != true {
		t.Fatalf("command response missing %s: %#v", automatedMixinKey, raw)
	}
	if client.callCount != 0 {
		t.Fatalf("invalid command path made %d A2A calls", client.callCount)
	}
}

func TestAgentsSlashCommandUsesGalleryRenderer(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, recorder := pollingHarness(t, client)
	prepareDirectoryBot(t, b, evt.RoomID)
	event := directoryEventWithBody("$slash-agents", evt.Sender, evt.RoomID, "/agents k8s")
	b.HandleMessage(t.Context(), event)

	events := recorder.snapshot()
	if len(events) != 1 || !containsAll(events[0].Body, "@agent-k8s:"+ownServer, "Type: local") {
		t.Fatalf("/agents detail = %#v", events)
	}
	if client.callCount != 0 || len(client.cardPaths) != 0 {
		t.Fatalf("/agents touched A2A: calls=%d cards=%v", client.callCount, client.cardPaths)
	}
}

func TestBudgetCommandReadsLimitsAndReservationsWithoutConsumption(t *testing.T) {
	agents, err := LoadAgents(writeTemp(t, validRemoteAgentsYAML))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	client := &scriptedA2AClient{remoteReady: true}
	b, _, evt, _, recorder := pollingHarness(t, client)
	prepareDirectoryBot(t, b, evt.RoomID)
	b.agents = agents
	clock := &limiterTestClock{now: time.Unix(1_700_000_000, 0)}
	b.senderLimits = newLimitersWithClock(60, 3, testRateLimitBucketCapacity, clock.Now)
	b.roomLimits = newLimitersWithClock(30, 10, testRateLimitBucketCapacity, clock.Now)
	sender := b.agents.IdentifySender(evt.Sender)
	if !b.senderLimits.Allow(sender.rateLimitKey("agent-remote")) || !b.roomLimits.Allow(evt.RoomID.String()) {
		t.Fatal("failed to consume invocation limiter fixtures")
	}

	event := directoryEventWithBody("$budget", evt.Sender, evt.RoomID, "/budget")
	b.HandleMessage(t.Context(), event)
	events := recorder.snapshot()
	if len(events) != 1 || !containsAll(
		events[0].Body,
		"Room invocation rate: 30/minute, burst 10; 9 whole request(s) available",
		"Sender + agent invocation rate: 60/minute, burst 3",
		"agent-remote: 2 whole request(s) available",
		"maxTokens 8192 per request",
		"reservation ceilings, not observed or spent token consumption",
	) {
		t.Fatalf("/budget response = %#v", events)
	}
	if got := b.senderLimits.snapshot(sender.rateLimitKey("agent-remote")).available; got != 2 {
		t.Fatalf("/budget changed sender availability to %d", got)
	}
	if got := b.roomLimits.snapshot(evt.RoomID.String()).available; got != 9 {
		t.Fatalf("/budget changed room availability to %d", got)
	}
	if client.callCount != 0 || len(client.cardPaths) != 0 {
		t.Fatalf("/budget touched A2A: calls=%d cards=%v", client.callCount, client.cardPaths)
	}
}

func TestAskCommandDurableIntakeUsesTheMentionJobContract(t *testing.T) {
	b := testBridge(t)
	body := transactionBody(
		t,
		transactionEvent("$ask-command", "@alice:"+ownServer, "/ask k8s inspect the pod"),
		transactionEvent("$ask-alias", "@alice:"+ownServer, "!ask k8s inspect the service"),
		transactionEvent("$bridged-command", "@slack_U123:"+ownServer, "/ask slack inspect the channel"),
		transactionEvent("$unknown-command", "@alice:"+ownServer, "/delegate k8s inspect"),
		transactionEvent("$foreign-agent", "@alice:"+ownServer, "/ask @agent-k8s:partner.example inspect"),
	)

	jobs, err := b.delegationsFromTransaction(body)
	if err != nil {
		t.Fatalf("delegationsFromTransaction: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("command jobs = %d, want 3", len(jobs))
	}
	want := []struct {
		eventID, ghost, prompt, origin string
	}{
		{"$ask-command", "agent-k8s", "inspect the pod", "matrix"},
		{"$ask-alias", "agent-k8s", "inspect the service", "matrix"},
		{"$bridged-command", "agent-slack", "inspect the channel", "bridge"},
	}
	for index, expected := range want {
		job := jobs[index]
		if job.MatrixEventID != expected.eventID || job.GhostLocalpart != expected.ghost ||
			job.Prompt != expected.prompt || job.SenderOriginKind != expected.origin ||
			job.TargetFingerprint == "" {
			t.Errorf("durable /ask job %d = %+v, want %+v", index, job, expected)
		}
	}
}

func TestAskCommandRejectsDeniedNativeSenderWithoutLeakingAgent(t *testing.T) {
	for _, agent := range []string{"missing", "locked"} {
		t.Run(agent, func(t *testing.T) {
			client := &scriptedA2AClient{}
			b, _, evt, _, recorder := pollingHarness(t, client)
			prepareDirectoryBot(t, b, evt.RoomID)
			command := directoryEventWithBody(
				id.EventID("$denied-"+agent), evt.Sender, evt.RoomID, "/ask "+agent+" restricted work",
			)
			b.HandleMessage(t.Context(), command)

			events := recorder.snapshot()
			if len(events) != 1 || events[0].Body != unknownCommandAgentText() || strings.Contains(events[0].Body, agent) {
				t.Fatalf("denied /ask response = %#v", events)
			}
			if client.callCount != 0 {
				t.Fatalf("denied /ask made %d A2A calls", client.callCount)
			}
		})
	}
}

func TestAskCommandRejectsForeignFullMXID(t *testing.T) {
	b := testBridge(t)
	evt := &event.Event{Sender: id.NewUserID("alice", ownServer), RoomID: "!room:" + ownServer}
	command := parsePlaintextCommand("/ask @agent-k8s:partner.example inspect")
	if targets, known := b.resolveAskCommand(evt, command); known || len(targets.allowed) != 0 {
		t.Fatalf("foreign command target = %+v, known = %v", targets, known)
	}
}
