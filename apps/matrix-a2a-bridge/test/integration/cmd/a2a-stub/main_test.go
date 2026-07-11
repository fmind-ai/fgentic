package main

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func TestParseLoadMarker(t *testing.T) {
	record, ok := parseLoadMarker("provenance\nload room=07 seq=09\nend")
	if !ok || record.Room != 7 || record.Sequence != 9 {
		t.Fatalf("parseLoadMarker() = %+v, %v", record, ok)
	}
	if _, ok := parseLoadMarker("ordinary integration prompt"); ok {
		t.Fatal("ordinary prompt was classified as a load request")
	}
}

func TestStatsRecorderTracksConcurrencyAndOrder(t *testing.T) {
	recorder := &statsRecorder{}
	first := requestRecord{Room: 1, Sequence: 0}
	second := requestRecord{Room: 2, Sequence: 0}
	recorder.start(first)
	recorder.start(second)
	recorder.finish(first, true)
	recorder.finish(second, true)

	stats := recorder.snapshot()
	if stats.Active != 0 || stats.MaxActive != 2 || stats.TotalStarted != 2 || stats.TotalCompleted != 2 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(stats.Starts) != 2 || stats.Starts[0] != first || stats.Starts[1] != second {
		t.Fatalf("start order = %+v", stats.Starts)
	}
	if len(stats.Completions) != 2 || stats.Completions[0] != first || stats.Completions[1] != second {
		t.Fatalf("completion order = %+v", stats.Completions)
	}
}

func TestLoadDelayValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "disabled", value: "0s"},
		{name: "minimum", value: "2s", want: 2 * time.Second},
		{name: "maximum", value: "5s", want: 5 * time.Second},
		{name: "too short", value: "1s", wantErr: true},
		{name: "too long", value: "6s", wantErr: true},
		{name: "invalid", value: "slow", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("A2A_STUB_DELAY", test.value)
			got, err := loadDelay()
			if (err != nil) != test.wantErr {
				t.Fatalf("loadDelay() error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Errorf("loadDelay() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestMessageTextAndCancelledDelay(t *testing.T) {
	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("first"), a2a.NewTextPart(" second"))
	if got := messageText(message); got != "first second" {
		t.Fatalf("messageText() = %q", got)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitDelay(ctx, time.Hour); err == nil {
		t.Fatal("waitDelay() succeeded with a cancelled context")
	}
}
