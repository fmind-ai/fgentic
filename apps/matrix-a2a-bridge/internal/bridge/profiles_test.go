package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/id"

	"github.com/fmind/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind/matrix-a2a-bridge/internal/config"
	"github.com/fmind/matrix-a2a-bridge/internal/state"
)

func TestProfileFromCardPrefersLiveMetadata(t *testing.T) {
	agents, err := LoadAgents(writeTemp(t, `agents:
  agent-k8s:
    namespace: kagent
    name: k8s-cluster-agent
    description: Startup fallback
    avatarURL: mxc://fgentic.fmind.ai/k8s-avatar
`))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	ref, _ := agents.Lookup("agent-k8s")
	card := &a2a.AgentCard{
		Name:        "  Kubernetes\n  Specialist ",
		Description: "Diagnoses   cluster health and safely explains remediation. ",
		Skills: []a2a.AgentSkill{
			{ID: "diagnose", Name: "Cluster diagnosis"},
			{ID: "read-manifests"},
			{Name: "   "},
		},
	}

	profile := profileFromCard(ref, card)

	if profile.DisplayName != "Kubernetes Specialist" {
		t.Errorf("DisplayName = %q", profile.DisplayName)
	}
	if profile.Description != "Diagnoses cluster health and safely explains remediation." {
		t.Errorf("Description = %q", profile.Description)
	}
	if want := []string{"Cluster diagnosis", "read-manifests"}; !slices.Equal(profile.Skills, want) {
		t.Errorf("Skills = %v, want %v", profile.Skills, want)
	}
	if got := profile.AvatarURL.String(); got != "mxc://fgentic.fmind.ai/k8s-avatar" {
		t.Errorf("AvatarURL = %q", got)
	}
	if profile.AgentPath != "/api/a2a/kagent/k8s-cluster-agent" || profile.Status != profileStatusLive {
		t.Errorf("profile routing/status = (%q, %q)", profile.AgentPath, profile.Status)
	}
}

type cardResponse struct {
	card *a2a.AgentCard
	err  error
}

type cardSequenceClient struct {
	mu        sync.Mutex
	responses []cardResponse
	paths     []string
	ready     bool
}

func (c *cardSequenceClient) ResolveAgentCard(_ context.Context, target a2aclient.Target) (*a2a.AgentCard, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.paths = append(c.paths, target.String())
	if len(c.responses) == 0 {
		return nil, errors.New("unexpected agent card resolution")
	}
	response := c.responses[0]
	c.responses = c.responses[1:]
	if target.IsRemote() {
		if response.err == nil {
			c.ready = true
		} else if errors.Is(response.err, a2aclient.ErrRemoteTargetUntrusted) {
			c.ready = false
		}
	}
	return response.card, response.err
}

func (*cardSequenceClient) Call(context.Context, a2aclient.Target, string, string) (a2aclient.Result, error) {
	return a2aclient.Result{}, errors.New("unexpected A2A delegation")
}

func (*cardSequenceClient) PollTask(context.Context, a2aclient.Target, string) (a2aclient.Result, error) {
	return a2aclient.Result{}, errors.New("unexpected A2A task poll")
}

func (*cardSequenceClient) CancelTask(context.Context, a2aclient.Target, string) error {
	return errors.New("unexpected A2A task cancel")
}

func (c *cardSequenceClient) IsReady(target a2aclient.Target) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !target.IsRemote() || c.ready
}

type profileWrite struct {
	ghost   id.UserID
	profile agentProfile
}

type recordingProfileWriter struct {
	mu      sync.Mutex
	prepare int
	writes  []profileWrite
}

func (w *recordingProfileWriter) Prepare(context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prepare++
	return nil
}

func (w *recordingProfileWriter) Apply(_ context.Context, ghost id.UserID, profile agentProfile) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	profile.Skills = slices.Clone(profile.Skills)
	w.writes = append(w.writes, profileWrite{ghost: ghost, profile: profile})
	return nil
}

func (w *recordingProfileWriter) snapshot() []profileWrite {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.writes)
}

func TestReloadAgentsRetainsLastKnownProfileAndConfig(t *testing.T) {
	agentsPath := writeTemp(t, `agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    description: Startup fallback
`)
	agents, err := LoadAgents(agentsPath)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	client := &cardSequenceClient{responses: []cardResponse{
		{card: &a2a.AgentCard{Name: "Live K8s Expert", Description: "Live card purpose"}},
		{err: errors.New("gateway unavailable")},
		{err: errors.New("new agent unavailable")},
	}}
	writer := &recordingProfileWriter{}
	cfg := config.Config{
		ServerName: ownServer, GhostPrefix: "agent-", Concurrency: 1,
		AgentsPath: agentsPath, AgentsReloadInterval: time.Hour,
		AgentCardRefreshInterval: time.Hour, RequestTimeout: time.Second,
		SenderRatePerMinute: 60, SenderRateBurst: 10, RoomRatePerMinute: 60, RoomRateBurst: 10,
		RateLimitBucketCapacity: testRateLimitBucketCapacity,
	}
	as := &appservice.AppService{Registration: &appservice.Registration{SenderLocalpart: "a2a-bridge"}}
	b := New(cfg, as, agents, client, state.NewMemory(), slog.Default())
	b.profileWriter = writer

	b.syncProfiles(t.Context(), b.agents.Entries())
	profile, _ := b.profiles.get("agent-k8s")
	if profile.Status != profileStatusLive || profile.Description != "Live card purpose" {
		t.Fatalf("initial profile = %+v", profile)
	}

	writeAgentsFile(t, agentsPath, `agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    description: Changed fallback must not replace live metadata
    allowedSenders: ["@admin:fgentic.fmind.ai"]
`)
	reloaded, err := b.reloadAgents(t.Context())
	if err != nil || !reloaded {
		t.Fatalf("reloadAgents() = (%v, %v), want (true, nil)", reloaded, err)
	}
	profile, _ = b.profiles.get("agent-k8s")
	if profile.Status != profileStatusCached || profile.Description != "Live card purpose" || profile.DisplayName != "Live K8s Expert" {
		t.Fatalf("cached profile = %+v, want retained live metadata", profile)
	}
	ref, _ := b.agents.Lookup("agent-k8s")
	if ref.AllowsSender(b.agents.IdentifySender(id.NewUserID("alice", ownServer)), ownServer) {
		t.Fatal("reloaded sender policy did not take effect")
	}

	writeAgentsFile(t, agentsPath, "schemaVersion: 99\nagents:\n  agent-k8s: {namespace: kagent, name: replacement-agent}\n")
	if reloaded, err = b.reloadAgents(t.Context()); err == nil || reloaded {
		t.Fatalf("unknown-major reload = (%v, %v), want (false, error)", reloaded, err)
	}
	if !strings.Contains(err.Error(), "unsupported schemaVersion 99") {
		t.Fatalf("unknown-major reload error = %v", err)
	}
	ref, ok := b.agents.Lookup("agent-k8s")
	if !ok || ref.AllowsSender(b.agents.IdentifySender(id.NewUserID("alice", ownServer)), ownServer) {
		t.Fatal("invalid reload replaced the last-known routing policy")
	}

	writeAgentsFile(t, agentsPath, `agents:
  agent-k8s:
    namespace: kagent
    name: replacement-agent
    description: Replacement startup fallback
`)
	reloaded, err = b.reloadAgents(t.Context())
	if err != nil || !reloaded {
		t.Fatalf("remap reloadAgents() = (%v, %v), want (true, nil)", reloaded, err)
	}
	profile, _ = b.profiles.get("agent-k8s")
	if profile.Status != profileStatusFallback || profile.Description != "Replacement startup fallback" || profile.DisplayName != "Replacement Agent" {
		t.Fatalf("remapped fallback profile = %+v", profile)
	}

	writes := writer.snapshot()
	if len(writes) != 3 {
		t.Fatalf("profile writes = %d, want live, cached, and remapped fallback", len(writes))
	}
	if writes[1].profile.Status != profileStatusCached || writes[2].profile.Status != profileStatusFallback {
		t.Fatalf("profile write statuses = %q, %q", writes[1].profile.Status, writes[2].profile.Status)
	}
	if want := []string{
		"/api/a2a/kagent/k8s-agent",
		"/api/a2a/kagent/k8s-agent",
		"/api/a2a/kagent/replacement-agent",
	}; !slices.Equal(client.paths, want) {
		t.Fatalf("AgentCard paths = %v, want %v", client.paths, want)
	}
}

func TestStartWatchesAgentConfigAndRefreshesProfiles(t *testing.T) {
	agentsPath := writeTemp(t, `agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    description: Startup fallback
`)
	agents, err := LoadAgents(agentsPath)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	client := &cardSequenceClient{responses: []cardResponse{
		{card: &a2a.AgentCard{Name: "Kubernetes Specialist", Description: "Initial live purpose"}},
		{card: &a2a.AgentCard{Name: "Kubernetes Specialist", Description: "Reloaded live purpose"}},
	}}
	writer := &recordingProfileWriter{}
	cfg := config.Config{
		ServerName: ownServer, GhostPrefix: "agent-", Concurrency: 1,
		AgentsPath: agentsPath, AgentsReloadInterval: 5 * time.Millisecond,
		AgentCardRefreshInterval: time.Hour, RequestTimeout: time.Second,
		SenderRatePerMinute: 60, SenderRateBurst: 10, RoomRatePerMinute: 60, RoomRateBurst: 10,
		RateLimitBucketCapacity: testRateLimitBucketCapacity,
	}
	as := &appservice.AppService{Registration: &appservice.Registration{SenderLocalpart: "a2a-bridge"}}
	b := New(cfg, as, agents, client, state.NewMemory(), slog.Default())
	b.profileWriter = writer
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	writeAgentsFile(t, agentsPath, `agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    description: Changed fallback
    allowedSenders: ["@alice:fgentic.fmind.ai"]
`)
	deadline := time.NewTimer(2 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		profile, _ := b.profiles.get("agent-k8s")
		if profile.Description == "Reloaded live purpose" {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("profile did not reload: %+v", profile)
		case <-ticker.C:
		}
	}
	b.Stop()
	if err := ctx.Err(); err != nil {
		t.Fatalf("Bridge.Stop canceled the delegation parent context: %v", err)
	}

	writer.mu.Lock()
	prepareCalls := writer.prepare
	writer.mu.Unlock()
	if prepareCalls != 1 {
		t.Fatalf("profile writer Prepare calls = %d, want 1", prepareCalls)
	}
	if writes := writer.snapshot(); len(writes) != 2 || writes[1].profile.Description != "Reloaded live purpose" {
		t.Fatalf("profile writes = %+v", writes)
	}
}

func TestRemoteCardTrustFailureQuarantinesAndRemovesDirectoryEntry(t *testing.T) {
	agents, err := LoadAgents(writeTemp(t, validRemoteAgentsYAML))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	client := &cardSequenceClient{responses: []cardResponse{
		{card: &a2a.AgentCard{Name: "Verified Partner", Description: "Signed purpose"}},
		{err: fmt.Errorf("tampered fixture: %w", a2aclient.ErrRemoteTargetUntrusted)},
		{err: errors.New("network unavailable after quarantine")},
	}}
	cfg := config.Config{
		ServerName: ownServer, GhostPrefix: "agent-", Concurrency: 1,
		RequestTimeout: time.Second, AgentsReloadInterval: time.Hour,
		AgentCardRefreshInterval: time.Hour,
		SenderRatePerMinute:      60, SenderRateBurst: 10, RoomRatePerMinute: 60, RoomRateBurst: 10,
		RateLimitBucketCapacity: testRateLimitBucketCapacity,
	}
	as := &appservice.AppService{Registration: &appservice.Registration{SenderLocalpart: "a2a-bridge"}}
	b := New(cfg, as, agents, client, state.NewMemory(), slog.Default())
	b.profileWriter = nil

	if err := b.syncProfilesChecked(t.Context(), agents.Entries(), true); err != nil {
		t.Fatalf("initial remote sync: %v", err)
	}
	profile, _ := b.profiles.get("agent-remote")
	if profile.Status != profileStatusLive || profile.Description != "Signed purpose" {
		t.Fatalf("verified profile = %+v", profile)
	}
	if err := b.syncProfilesChecked(t.Context(), agents.Entries(), true); !errors.Is(err, a2aclient.ErrRemoteTargetUntrusted) {
		t.Fatalf("tampered remote sync error = %v", err)
	}
	profile, _ = b.profiles.get("agent-remote")
	if profile.Status != profileStatusRejected || profile.Description == "Signed purpose" || client.IsReady(profileTarget(t, agents, "agent-remote")) {
		t.Fatalf("quarantined profile = %+v, ready=%v", profile, client.ready)
	}
	if directory := b.agentDirectoryText(id.NewUserID("alice", ownServer)); strings.Contains(directory, "@agent-remote:") {
		t.Fatalf("directory advertises rejected target: %s", directory)
	}
	if err := b.syncProfilesChecked(t.Context(), agents.Entries(), true); err == nil {
		t.Fatal("post-quarantine network failure unexpectedly succeeded")
	}
	profile, _ = b.profiles.get("agent-remote")
	if profile.Status != profileStatusUnavailable {
		t.Fatalf("post-quarantine profile status = %q, want unavailable", profile.Status)
	}
	if directory := b.agentDirectoryText(id.NewUserID("alice", ownServer)); strings.Contains(directory, "@agent-remote:") {
		t.Fatalf("directory advertises unavailable target: %s", directory)
	}
}

func TestAgentCardRejectReasonDistinguishesExtensionGap(t *testing.T) {
	extErr := fmt.Errorf("card requires unsupported extension: %w", a2aclient.ErrRemoteExtensionUnsupported)
	if got := agentCardRejectReason(extErr); got != "agent_card_extension_unsupported" {
		t.Fatalf("extension gap reason = %q", got)
	}
	if got := agentCardRejectReason(fmt.Errorf("bad signature: %w", a2aclient.ErrRemoteTargetUntrusted)); got != "agent_card_untrusted" {
		t.Fatalf("generic trust failure reason = %q", got)
	}
}

func TestRemoteExtensionGapAuditsDistinctReason(t *testing.T) {
	agents, err := LoadAgents(writeTemp(t, validRemoteAgentsYAML))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	client := &cardSequenceClient{responses: []cardResponse{{
		err: fmt.Errorf("card requires unsupported extension: %w", a2aclient.ErrRemoteExtensionUnsupported),
	}}}
	cfg := config.Config{
		ServerName: ownServer, GhostPrefix: "agent-", Concurrency: 1,
		RequestTimeout: time.Second, AgentsReloadInterval: time.Hour,
		AgentCardRefreshInterval: time.Hour,
		SenderRatePerMinute:      60, SenderRateBurst: 10, RoomRatePerMinute: 60, RoomRateBurst: 10,
		RateLimitBucketCapacity: testRateLimitBucketCapacity,
	}
	as := &appservice.AppService{Registration: &appservice.Registration{SenderLocalpart: "a2a-bridge"}}
	b := New(cfg, as, agents, client, state.NewMemory(), slog.Default())
	b.profileWriter = nil
	var output strings.Builder
	setBridgeLogOutput(b, &output)

	if err := b.syncProfilesChecked(t.Context(), agents.Entries(), true); !errors.Is(err, a2aclient.ErrRemoteExtensionUnsupported) {
		t.Fatalf("sync err = %v, want ErrRemoteExtensionUnsupported", err)
	}
	if got := agentCardAuditReason(t, output.String()); got != "agent_card_extension_unsupported" {
		t.Fatalf("agent card audit reason = %q, want agent_card_extension_unsupported", got)
	}
}

// agentCardAuditReason returns the reason of the single agent-card audit record in the captured log.
func agentCardAuditReason(t *testing.T, output string) string {
	t.Helper()
	var reason string
	found := 0
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log record: %v", err)
		}
		if record["audit_schema"] == "fgentic.agent_card.v1" {
			found++
			reason, _ = record["reason"].(string)
		}
	}
	if found != 1 {
		t.Fatalf("agent card audit records = %d, want 1", found)
	}
	return reason
}

func TestStartFailsClosedForUnverifiedRemoteTarget(t *testing.T) {
	agents, err := LoadAgents(writeTemp(t, validRemoteAgentsYAML))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	client := &cardSequenceClient{responses: []cardResponse{{
		err: fmt.Errorf("unsigned fixture: %w", a2aclient.ErrRemoteTargetUntrusted),
	}}}
	cfg := config.Config{
		ServerName: ownServer, GhostPrefix: "agent-", Concurrency: 1,
		RequestTimeout: time.Second, AgentsReloadInterval: time.Hour,
		AgentCardRefreshInterval: time.Hour,
		SenderRatePerMinute:      60, SenderRateBurst: 10, RoomRatePerMinute: 60, RoomRateBurst: 10,
		RateLimitBucketCapacity: testRateLimitBucketCapacity,
	}
	as := &appservice.AppService{Registration: &appservice.Registration{SenderLocalpart: "a2a-bridge"}}
	b := New(cfg, as, agents, client, state.NewMemory(), slog.Default())
	b.profileWriter = nil

	if err := b.Start(t.Context()); !errors.Is(err, a2aclient.ErrRemoteTargetUntrusted) {
		t.Fatalf("Start() error = %v, want remote trust failure", err)
	}
	profile, _ := b.profiles.get("agent-remote")
	if profile.Status != profileStatusRejected {
		t.Fatalf("startup profile status = %q, want rejected", profile.Status)
	}
	b.Stop()
}

func TestReloadRejectsRemotePreflightErrorEvenWithOldReadyCache(t *testing.T) {
	agentsPath := writeTemp(t, `agents:
  agent-local:
    namespace: kagent
    name: local-agent
`)
	agents, err := LoadAgents(agentsPath)
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	client := &cardSequenceClient{
		ready:     true,
		responses: []cardResponse{{err: errors.New("remote card network failure")}},
	}
	cfg := config.Config{
		ServerName: ownServer, GhostPrefix: "agent-", Concurrency: 1,
		AgentsPath: agentsPath, RequestTimeout: time.Second,
		AgentsReloadInterval: time.Hour, AgentCardRefreshInterval: time.Hour,
		SenderRatePerMinute: 60, SenderRateBurst: 10, RoomRatePerMinute: 60, RoomRateBurst: 10,
		RateLimitBucketCapacity: testRateLimitBucketCapacity,
	}
	as := &appservice.AppService{Registration: &appservice.Registration{SenderLocalpart: "a2a-bridge"}}
	b := New(cfg, as, agents, client, state.NewMemory(), slog.Default())
	b.profileWriter = nil
	writeAgentsFile(t, agentsPath, validRemoteAgentsYAML)

	reloaded, err := b.reloadAgents(t.Context())
	if err == nil || reloaded {
		t.Fatalf("reloadAgents() = (%v, %v), want (false, error)", reloaded, err)
	}
	if _, ok := b.agents.Lookup("agent-local"); !ok {
		t.Fatal("failed remote preflight replaced the last-known local mapping")
	}
	if _, ok := b.agents.Lookup("agent-remote"); ok {
		t.Fatal("failed remote candidate became routable")
	}
}

func profileTarget(t *testing.T, agents *AgentMap, ghost string) a2aclient.Target {
	t.Helper()
	ref, ok := agents.Lookup(ghost)
	if !ok {
		t.Fatalf("agent %s missing", ghost)
	}
	return ref.Target()
}

func writeAgentsFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write agents file: %v", err)
	}
}

type matrixProfileRequest struct {
	method string
	path   string
	body   map[string]any
}

func TestMatrixProfileWriterUsesStandardProfileAPIs(t *testing.T) {
	var mu sync.Mutex
	var requests []matrixProfileRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/_matrix/client/versions":
			_, _ = w.Write([]byte(`{"versions":["v1.16"]}`))
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/register"):
			_, _ = w.Write([]byte("{}"))
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/displayname"):
			_, _ = w.Write([]byte(`{"displayname":""}`))
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/avatar_url"):
			_, _ = w.Write([]byte(`{"avatar_url":""}`))
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/download/"):
			// IntentAPI probes the media before setting it for homeserver compatibility. A failed
			// probe is explicitly non-fatal in mautrix and does not change the profile API call.
			http.NotFound(w, req)
		case req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/profile/"):
			body := make(map[string]any)
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			requests = append(requests, matrixProfileRequest{method: req.Method, path: req.URL.Path, body: body})
			mu.Unlock()
			_, _ = w.Write([]byte("{}"))
		default:
			http.Error(w, fmt.Sprintf("unexpected request: %s %s", req.Method, req.URL.Path), http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	as, err := appservice.CreateFull(appservice.CreateOpts{
		Registration:     &appservice.Registration{AppToken: "test-token", SenderLocalpart: "a2a-bridge"},
		HomeserverDomain: ownServer,
		HomeserverURL:    server.URL,
	})
	if err != nil {
		t.Fatalf("CreateFull: %v", err)
	}
	as.HTTPClient = server.Client()
	as.DefaultHTTPRetries = 0
	writer := &matrixProfileWriter{as: as, log: slog.Default()}
	if err := writer.Prepare(t.Context()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	avatar, err := id.ParseContentURI("mxc://fgentic.fmind.ai/k8s-avatar")
	if err != nil {
		t.Fatalf("ParseContentURI: %v", err)
	}
	profile := agentProfile{
		DisplayName: "Kubernetes Specialist",
		Description: "Diagnoses cluster health.",
		Skills:      []string{"Diagnosis", "Read manifests"},
		AvatarURL:   avatar,
		AgentPath:   "/api/a2a/kagent/k8s-agent",
		Status:      profileStatusLive,
	}
	if err := writer.Apply(t.Context(), id.NewUserID("agent-k8s", ownServer), profile); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	mu.Lock()
	got := slices.Clone(requests)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("profile PUT requests = %d, want displayname, avatar, and %s: %+v", len(got), agentProfileField, got)
	}
	if !strings.HasSuffix(got[0].path, "/displayname") || got[0].body["displayname"] != profile.DisplayName {
		t.Errorf("displayname request = %+v", got[0])
	}
	if !strings.HasSuffix(got[1].path, "/avatar_url") || got[1].body["avatar_url"] != profile.AvatarURL.String() {
		t.Errorf("avatar request = %+v", got[1])
	}
	if !strings.HasSuffix(got[2].path, "/"+agentProfileField) {
		t.Fatalf("custom profile path = %q", got[2].path)
	}
	metadata, ok := got[2].body[agentProfileField].(map[string]any)
	if !ok {
		t.Fatalf("custom profile body = %#v", got[2].body)
	}
	if metadata["description"] != profile.Description {
		t.Errorf("custom description = %#v", metadata["description"])
	}
	if skills, ok := metadata["skills"].([]any); !ok || len(skills) != 2 {
		t.Errorf("custom skills = %#v", metadata["skills"])
	}
}
