package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/agentcardjws"
)

func TestReleaseGate(t *testing.T) {
	gate := newReleaseGate(true)
	heldCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gate.wait(heldCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("enabled releaseGate.wait() error = %v, want context cancellation", err)
	}

	gate.release()
	gate.release()
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("released releaseGate.wait() error = %v", err)
	}
}

func TestLoadHold(t *testing.T) {
	t.Setenv("A2A_STUB_HOLD", "true")
	hold, err := loadHold()
	if err != nil || !hold {
		t.Fatalf("loadHold() = %v, %v", hold, err)
	}
	t.Setenv("A2A_STUB_HOLD", "invalid")
	if _, err := loadHold(); err == nil {
		t.Fatal("loadHold() accepted an invalid boolean")
	}
}

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

func TestRecordRemoteUserCapturesMatrixAttributionOnCallsOnly(t *testing.T) {
	recorder := &statsRecorder{}
	handler := recordRemoteUser(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), recorder)

	cardRequest := httptest.NewRequest(http.MethodGet, "/.well-known/agent-card.json", nil)
	cardRequest.Header.Set("X-User-Id", "@card-fetch:integration.test")
	handler.ServeHTTP(httptest.NewRecorder(), cardRequest)
	if got := recorder.snapshot().RemoteUserID; got != "" {
		t.Fatalf("AgentCard request attribution = %q, want empty", got)
	}

	callRequest := httptest.NewRequest(http.MethodPost, "/api/a2a/remote-agent", nil)
	callRequest.Header.Set("X-User-Id", "@alice:integration.test")
	handler.ServeHTTP(httptest.NewRecorder(), callRequest)
	if got := recorder.snapshot().RemoteUserID; got != "@alice:integration.test" {
		t.Fatalf("A2A request attribution = %q", got)
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

func TestSignedRemoteAgentCardHasStableIdentityAndIsTamperEvident(t *testing.T) {
	t.Parallel()

	first, err := signedRemoteAgentCard("http://a2a-stub:8080")
	if err != nil {
		t.Fatalf("signedRemoteAgentCard() first: %v", err)
	}
	second, err := signedRemoteAgentCard("http://a2a-stub:8080")
	if err != nil {
		t.Fatalf("signedRemoteAgentCard() second: %v", err)
	}
	if len(first.Skills) != 1 || first.Skills[0].ID != "echo" || len(first.Skills[0].Tags) == 0 {
		t.Fatalf("signed AgentCard skills = %#v, want one complete echo skill", first.Skills)
	}
	unsignedFirst := *first
	unsignedFirst.Signatures = nil
	firstJSON, err := json.Marshal(&unsignedFirst)
	if err != nil {
		t.Fatalf("json.Marshal(first): %v", err)
	}
	unsignedSecond := *second
	unsignedSecond.Signatures = nil
	secondJSON, err := json.Marshal(&unsignedSecond)
	if err != nil {
		t.Fatalf("json.Marshal(second): %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatal("fixed fixture identity did not produce stable AgentCard content")
	}
	if !validFixtureSignature(t, first) {
		t.Fatal("signed AgentCard did not verify with the fixture public key")
	}
	if !validFixtureSignature(t, second) {
		t.Fatal("second signed AgentCard did not verify with the fixture public key")
	}

	tampered := *first
	tampered.Name += " (tampered after signing)"
	if validFixtureSignature(t, &tampered) {
		t.Fatal("post-signature AgentCard tampering still verified")
	}
}

func TestFixturePublicJWKMatchesIntegrationConfig(t *testing.T) {
	t.Parallel()

	privateKey := fixturePrivateKey()
	const coordinateBytes = 32
	x := base64.RawURLEncoding.EncodeToString(privateKey.X.FillBytes(make([]byte, coordinateBytes)))
	y := base64.RawURLEncoding.EncodeToString(privateKey.Y.FillBytes(make([]byte, coordinateBytes)))
	if want := "axfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpY"; x != want {
		t.Errorf("fixture JWK x = %q, want %q", x, want)
	}
	if want := "T-NC4v4af5uO5-tKfA-eFivOM1drMV7Oy7ZAaDe_UfU"; y != want {
		t.Errorf("fixture JWK y = %q, want %q", y, want)
	}
}

func TestValidTokenBudgetContract(t *testing.T) {
	t.Parallel()

	execCtx := &a2asrv.ExecutorContext{
		Message: &a2a.Message{
			Extensions: []string{a2aclient.TokenBudgetExtensionURI},
			Metadata: map[string]any{
				a2aclient.TokenBudgetExtensionURI: map[string]any{
					"maxTokens": float64(integrationTokenBudget),
				},
			},
		},
		ServiceParams: a2asrv.NewServiceParams(map[string][]string{
			a2a.SvcParamExtensions: {a2aclient.TokenBudgetExtensionURI},
		}),
	}
	if !validTokenBudgetContract(execCtx) {
		t.Fatal("complete token-budget contract was rejected")
	}
	execCtx.Message.Metadata[a2aclient.TokenBudgetExtensionURI].(map[string]any)["maxTokens"] = float64(4095)
	if validTokenBudgetContract(execCtx) {
		t.Fatal("wrong maxTokens was accepted")
	}
}

func validFixtureSignature(t *testing.T, card *a2a.AgentCard) bool {
	t.Helper()
	encoded, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("encode AgentCard: %v", err)
	}
	document, err := agentcardjws.Parse(encoded)
	if err != nil {
		t.Fatalf("parse signed AgentCard: %v", err)
	}
	privateKey := fixturePrivateKey()
	return agentcardjws.Verify(document, &privateKey.PublicKey, remoteKeyID) == nil
}
