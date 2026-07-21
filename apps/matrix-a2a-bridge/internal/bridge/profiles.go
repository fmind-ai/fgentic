package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
)

const (
	agentProfileField        = "dev.fgentic.agent"
	maxProfileNameRunes      = 128
	maxDescriptionRunes      = 512
	maxProfileSkillCount     = 20
	maxProfileSkillRunes     = 128
	profileStatusLive        = profileStatus("live")
	profileStatusCached      = profileStatus("cached")
	profileStatusFallback    = profileStatus("fallback")
	profileStatusRejected    = profileStatus("rejected")
	profileStatusUnavailable = profileStatus("unavailable")
)

type profileStatus string

type agentProfile struct {
	DisplayName string
	Description string
	Skills      []string
	AvatarURL   id.ContentURI
	AgentPath   string
	MappingID   string
	Status      profileStatus
}

type profileStore struct {
	mu      sync.RWMutex
	byGhost map[string]agentProfile
}

func newProfileStore(entries []AgentEntry) *profileStore {
	store := &profileStore{byGhost: make(map[string]agentProfile, len(entries))}
	store.prepare(entries)
	return store
}

// prepare keeps a last-known card only while a ghost still maps to the same A2A path. A remap
// must not accidentally display metadata from the previous agent.
func (s *profileStore) prepare(entries []AgentEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]agentProfile, len(entries))
	for _, entry := range entries {
		current, ok := s.byGhost[entry.Ghost]
		if ok && current.MappingID == entry.Ref.MappingID() && current.Status != profileStatusFallback {
			current.AvatarURL = entry.Ref.Avatar()
			next[entry.Ghost] = current
			continue
		}
		next[entry.Ghost] = fallbackProfile(entry.Ref)
	}
	s.byGhost = next
}

func (s *profileStore) set(ghost string, profile agentProfile) {
	s.mu.Lock()
	s.byGhost[ghost] = profile
	s.mu.Unlock()
}

func (s *profileStore) get(ghost string) (agentProfile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	profile, ok := s.byGhost[ghost]
	profile.Skills = append([]string(nil), profile.Skills...)
	return profile, ok
}

// failedRefresh retains a last-known card for the same A2A path and marks it cached. New or
// remapped agents use their configured fallback until a card becomes available.
func (s *profileStore) failedRefresh(entry AgentEntry) agentProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	profile, ok := s.byGhost[entry.Ghost]
	if ok && profile.MappingID == entry.Ref.MappingID() && profile.Status != profileStatusFallback &&
		profile.Status != profileStatusRejected && profile.Status != profileStatusUnavailable {
		profile.Status = profileStatusCached
		profile.AvatarURL = entry.Ref.Avatar()
		s.byGhost[entry.Ghost] = profile
		return profile
	}
	profile = fallbackProfile(entry.Ref)
	s.byGhost[entry.Ghost] = profile
	return profile
}

// rejectedRefresh removes previously published card metadata after a cryptographic trust
// failure. Keeping a remote target quarantined while showing its last-known card as available
// would make the directory disagree with the actual dispatch boundary.
func (s *profileStore) rejectedRefresh(entry AgentEntry) agentProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	profile := fallbackProfile(entry.Ref)
	profile.Status = profileStatusRejected
	s.byGhost[entry.Ghost] = profile
	return profile
}

func (s *profileStore) unavailableRefresh(entry AgentEntry) agentProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	profile := fallbackProfile(entry.Ref)
	profile.Status = profileStatusUnavailable
	s.byGhost[entry.Ghost] = profile
	return profile
}

func fallbackProfile(ref *AgentRef) agentProfile {
	return agentProfile{
		DisplayName: humanizeAgentName(ref.Name),
		Description: normalizeProfileText(ref.Description, maxDescriptionRunes),
		AvatarURL:   ref.Avatar(),
		AgentPath:   ref.Path(),
		MappingID:   ref.MappingID(),
		Status:      profileStatusFallback,
	}
}

func profileFromCard(ref *AgentRef, card *a2a.AgentCard) agentProfile {
	profile := fallbackProfile(ref)
	profile.Status = profileStatusLive
	if name := normalizeProfileText(card.Name, maxProfileNameRunes); name != "" {
		profile.DisplayName = name
	}
	if description := normalizeProfileText(card.Description, maxDescriptionRunes); description != "" {
		profile.Description = description
	}
	profile.Skills = make([]string, 0, min(len(card.Skills), maxProfileSkillCount))
	for _, skill := range card.Skills {
		if len(profile.Skills) == maxProfileSkillCount {
			break
		}
		name := normalizeProfileText(skill.Name, maxProfileSkillRunes)
		if name == "" {
			name = normalizeProfileText(skill.ID, maxProfileSkillRunes)
		}
		if name != "" {
			profile.Skills = append(profile.Skills, name)
		}
	}
	return profile
}

func normalizeProfileText(value string, maxRunes int) string {
	normalized := strings.Join(strings.Fields(value), " ")
	runes := []rune(normalized)
	if len(runes) <= maxRunes {
		return normalized
	}
	return string(runes[:maxRunes-1]) + "…"
}

func humanizeAgentName(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	for i, part := range parts {
		runes := []rune(part)
		if len(runes) > 0 {
			runes[0] = unicode.ToUpper(runes[0])
			parts[i] = string(runes)
		}
	}
	return normalizeProfileText(strings.Join(parts, " "), maxProfileNameRunes)
}

type ghostProfileWriter interface {
	Prepare(context.Context) error
	Apply(context.Context, id.UserID, agentProfile) error
}

type matrixProfileWriter struct {
	as                  *appservice.AppService
	log                 *slog.Logger
	customProfileFields bool
}

func (w *matrixProfileWriter) Prepare(ctx context.Context) error {
	botClient := w.as.BotClient()
	versions, err := botClient.Versions(ctx)
	if err != nil {
		return fmt.Errorf("query Matrix versions for profile sync: %w", err)
	}
	w.as.SpecVersions = versions
	botClient.SpecVersions = versions
	w.customProfileFields = versions.Supports(mautrix.FeatureArbitraryProfileFields)
	if !w.customProfileFields {
		w.log.Info("homeserver lacks Matrix v1.16 arbitrary profile fields; agent descriptions remain available through !agents")
	}
	return nil
}

func (w *matrixProfileWriter) Apply(ctx context.Context, ghost id.UserID, profile agentProfile) error {
	intent := w.as.Intent(ghost)
	if intent == nil {
		return fmt.Errorf("create Matrix intent for %s", ghost)
	}
	var errs []error
	if err := intent.SetDisplayName(ctx, profile.DisplayName); err != nil {
		errs = append(errs, fmt.Errorf("set display name for %s: %w", ghost, err))
	}
	// Reconcile the avatar to the configured state. intent.SetAvatarURL reads the ghost's current
	// avatar and is a no-op when unchanged, so passing the empty URI of a removed avatarURL clears
	// stale Matrix avatar state without clobbering or issuing a redundant write (#89).
	if err := intent.SetAvatarURL(ctx, profile.AvatarURL); err != nil {
		errs = append(errs, fmt.Errorf("set avatar for %s: %w", ghost, err))
	}
	if w.customProfileFields {
		metadata := struct {
			Description string   `json:"description"`
			Skills      []string `json:"skills,omitempty"`
		}{Description: profile.Description, Skills: profile.Skills}
		if err := intent.SetProfileField(ctx, agentProfileField, metadata); err != nil {
			errs = append(errs, fmt.Errorf("set agent metadata for %s: %w", ghost, err))
		}
	}
	return errors.Join(errs...)
}

func (b *Bridge) syncProfiles(ctx context.Context, entries []AgentEntry) {
	_ = b.syncProfilesChecked(ctx, entries, false)
}

// syncProfilesChecked refreshes cards in parallel and returns only remote-target failures.
// Local AgentCards are presentation metadata and retain their historical best-effort behavior;
// remote cards are an authorization boundary and are therefore surfaced to startup/reload.
func (b *Bridge) syncProfilesChecked(ctx context.Context, entries []AgentEntry, remoteOnly bool) error {
	parallelism := max(1, b.cfg.Concurrency)
	sem := make(chan struct{}, parallelism)
	errs := make(chan error, len(entries))
	var wg sync.WaitGroup
	for _, entry := range entries {
		if remoteOnly && !entry.Ref.Target().IsRemote() {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				if entry.Ref.Target().IsRemote() {
					errs <- fmt.Errorf("refresh remote AgentCard for %s: %w", entry.Ghost, ctx.Err())
				}
				return
			}
			if err := b.syncProfile(ctx, entry); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	var failures []error
	for err := range errs {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (b *Bridge) syncProfile(ctx context.Context, entry AgentEntry) error {
	target := entry.Ref.Target()
	card, err := b.verifyRemoteCard(ctx, entry)
	if err == nil && card == nil {
		return nil
	}
	if err != nil {
		previous, _ := b.profiles.get(entry.Ghost)
		untrusted := target.IsRemote() && errors.Is(err, a2aclient.ErrRemoteTargetUntrusted)
		unavailable := target.IsRemote() && (b.client == nil || !b.client.IsReady(target))
		profile := agentProfile{}
		switch {
		case untrusted:
			profile = b.profiles.rejectedRefresh(entry)
			if previous.Status != profileStatusRejected {
				b.logAgentCardAudit(entry, "rejected", agentCardRejectReason(err))
			}
			b.log.Error("remote agent card rejected; target quarantined",
				"ghost", entry.Ghost, "agent", entry.Ref.Path(), "profile_status", profile.Status, "err", err)
		case unavailable:
			profile = b.profiles.unavailableRefresh(entry)
			b.log.Error("remote agent card unavailable; target has no verified client",
				"ghost", entry.Ghost, "agent", entry.Ref.Path(), "profile_status", profile.Status, "err", err)
		default:
			profile = b.profiles.failedRefresh(entry)
			b.log.Warn("agent card refresh failed; retaining last-known profile",
				"ghost", entry.Ghost, "agent", entry.Ref.Path(), "profile_status", profile.Status, "err", err)
		}
		b.applyGhostProfileWithTimeout(ctx, entry.Ghost, profile)
		if target.IsRemote() {
			return fmt.Errorf("refresh remote AgentCard for %s: %w", entry.Ghost, err)
		}
		return nil
	}
	previous, _ := b.profiles.get(entry.Ghost)
	profile := profileFromCard(entry.Ref, card)
	b.profiles.set(entry.Ghost, profile)
	b.applyGhostProfileWithTimeout(ctx, entry.Ghost, profile)
	if target.IsRemote() && previous.Status != profileStatusLive {
		b.logAgentCardAudit(entry, "accepted", "agent_card_verified")
	}
	return nil
}

// verifyRemoteCard owns card resolution and the remote readiness postcondition used by both
// profile refresh and configuration-reload preflight. A nil local client keeps profile sync
// disabled; a nil remote client fails closed.
func (b *Bridge) verifyRemoteCard(ctx context.Context, entry AgentEntry) (*a2a.AgentCard, error) {
	target := entry.Ref.Target()
	if b.client == nil {
		if target.IsRemote() {
			return nil, fmt.Errorf("A2A client is unavailable")
		}
		return nil, nil
	}
	cardCtx, cancel := context.WithTimeout(ctx, agentRequestTimeout(entry.Ref, b.cfg.RequestTimeout))
	defer cancel()
	card, err := b.client.ResolveAgentCard(cardCtx, target)
	if err != nil {
		return nil, err
	}
	if card == nil {
		return nil, errors.New("agent card resolver returned an empty card")
	}
	if target.IsRemote() && !b.client.IsReady(target) {
		return nil, fmt.Errorf(
			"remote card resolver did not install a verified client: %w",
			a2aclient.ErrRemoteTargetUntrusted,
		)
	}
	return card, nil
}

func agentRequestTimeout(ref *AgentRef, global time.Duration) time.Duration {
	if remote := ref.Timeout(); remote > 0 && remote < global {
		return remote
	}
	return global
}

// agentCardRejectReason distinguishes a required-extension negotiation gap from other trust
// failures, so operators can tell "the card demands a capability we are not configured to
// activate" apart from a signature, identity, or endpoint mismatch (docs/bridge.md §6).
func agentCardRejectReason(err error) string {
	if errors.Is(err, a2aclient.ErrRemoteExtensionUnsupported) {
		return "agent_card_extension_unsupported"
	}
	if errors.Is(err, a2aclient.ErrRemoteKeyRevoked) {
		return "agent_card_revoked"
	}
	return "agent_card_untrusted"
}

func (b *Bridge) logAgentCardAudit(entry AgentEntry, outcome, reason string) {
	b.auditLog.Info(
		"remote agent card audit",
		"audit_schema", "fgentic.agent_card.v1",
		"ghost", entry.Ghost,
		"agent_target", entry.Ref.Path(),
		"outcome", outcome,
		"reason", reason,
	)
}

func (b *Bridge) applyGhostProfileWithTimeout(ctx context.Context, ghost string, profile agentProfile) {
	profileCtx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout)
	defer cancel()
	b.applyGhostProfile(profileCtx, ghost, profile)
}

func (b *Bridge) applyGhostProfile(ctx context.Context, ghost string, profile agentProfile) {
	if b.profileWriter == nil {
		return
	}
	mxid := id.NewUserID(ghost, b.cfg.ServerName)
	if err := b.profileWriter.Apply(ctx, mxid, profile); err != nil {
		b.log.Error("sync Matrix ghost profile", "ghost", mxid, "agent", profile.AgentPath, "err", err)
		return
	}
	b.log.Info("synced Matrix ghost profile", "ghost", mxid, "agent", profile.AgentPath,
		"display_name", profile.DisplayName, "profile_status", profile.Status)
}

func (b *Bridge) reloadAgents(ctx context.Context) (bool, error) {
	next, err := LoadAgents(b.cfg.AgentsPath)
	if err != nil {
		return false, err
	}
	if b.agents.SameConfig(next) {
		return false, nil
	}
	entries := next.Entries()
	if err := b.preflightRemoteAgents(ctx, entries); err != nil {
		return false, err
	}
	next.LogSchemaVersionWarning(b.log, b.cfg.AgentsPath)
	b.agentConfigMu.Lock()
	b.agents.Replace(next)
	b.profiles.prepare(entries)
	b.agentConfigMu.Unlock()
	b.log.Info("reloaded agent routing map", "agents", b.agents.Names())
	b.syncProfiles(ctx, entries)
	return true, nil
}

func (b *Bridge) preflightRemoteAgents(ctx context.Context, entries []AgentEntry) error {
	var failures []error
	for _, entry := range entries {
		if !entry.Ref.Target().IsRemote() {
			continue
		}
		_, err := b.verifyRemoteCard(ctx, entry)
		if err != nil {
			if errors.Is(err, a2aclient.ErrRemoteTargetUntrusted) {
				b.logAgentCardAudit(entry, "rejected", agentCardRejectReason(err))
			}
			failures = append(failures, fmt.Errorf("verify remote AgentCard for %s: %w", entry.Ghost, err))
		}
	}
	return errors.Join(failures...)
}

func (b *Bridge) watchAgents(ctx context.Context) {
	defer b.watchWG.Done()
	agentsTicker := time.NewTicker(b.cfg.AgentsReloadInterval)
	defer agentsTicker.Stop()
	cardTicker := time.NewTicker(b.cfg.AgentCardRefreshInterval)
	defer cardTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-agentsTicker.C:
			if _, err := b.reloadAgents(ctx); err != nil {
				b.log.Error("reload agent routing map; keeping last-known config", "err", err)
			}
		case <-cardTicker.C:
			_ = b.syncProfilesChecked(ctx, b.agents.Entries(), true)
		}
	}
}
