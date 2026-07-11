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
)

const (
	agentProfileField     = "dev.fgentic.agent"
	maxProfileNameRunes   = 128
	maxDescriptionRunes   = 512
	maxProfileSkillCount  = 20
	maxProfileSkillRunes  = 128
	profileStatusLive     = profileStatus("live")
	profileStatusCached   = profileStatus("cached")
	profileStatusFallback = profileStatus("fallback")
)

type profileStatus string

type agentProfile struct {
	DisplayName string
	Description string
	Skills      []string
	AvatarURL   id.ContentURI
	AgentPath   string
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
		if ok && current.AgentPath == entry.Ref.Path() && current.Status != profileStatusFallback {
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
	if ok && profile.AgentPath == entry.Ref.Path() && profile.Status != profileStatusFallback {
		profile.Status = profileStatusCached
		profile.AvatarURL = entry.Ref.Avatar()
		s.byGhost[entry.Ghost] = profile
		return profile
	}
	profile = fallbackProfile(entry.Ref)
	s.byGhost[entry.Ghost] = profile
	return profile
}

func fallbackProfile(ref *AgentRef) agentProfile {
	return agentProfile{
		DisplayName: humanizeAgentName(ref.Name),
		Description: normalizeProfileText(ref.Description, maxDescriptionRunes),
		AvatarURL:   ref.Avatar(),
		AgentPath:   ref.Path(),
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
	if !profile.AvatarURL.IsEmpty() {
		if err := intent.SetAvatarURL(ctx, profile.AvatarURL); err != nil {
			errs = append(errs, fmt.Errorf("set avatar for %s: %w", ghost, err))
		}
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
	if b.client == nil {
		return
	}
	parallelism := max(1, b.cfg.Concurrency)
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for _, entry := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			b.syncProfile(ctx, entry)
		}()
	}
	wg.Wait()
}

func (b *Bridge) syncProfile(ctx context.Context, entry AgentEntry) {
	cardCtx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout)
	card, err := b.client.ResolveAgentCard(cardCtx, entry.Ref.Path())
	cancel()
	if err != nil {
		profile := b.profiles.failedRefresh(entry)
		b.log.Warn("agent card refresh failed; retaining last-known profile",
			"ghost", entry.Ghost, "agent", entry.Ref.Path(), "profile_status", profile.Status, "err", err)
		b.applyGhostProfileWithTimeout(ctx, entry.Ghost, profile)
		return
	}
	profile := profileFromCard(entry.Ref, card)
	b.profiles.set(entry.Ghost, profile)
	b.applyGhostProfileWithTimeout(ctx, entry.Ghost, profile)
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
	b.agentConfigMu.Lock()
	b.agents.Replace(next)
	b.profiles.prepare(entries)
	b.agentConfigMu.Unlock()
	b.log.Info("reloaded agent routing map", "agents", b.agents.Names())
	b.syncProfiles(ctx, entries)
	return true, nil
}

func (b *Bridge) watchAgents(ctx context.Context) {
	defer b.watchWG.Done()
	ticker := time.NewTicker(b.cfg.AgentsReloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := b.reloadAgents(ctx); err != nil {
				b.log.Error("reload agent routing map; keeping last-known config", "err", err)
			}
		}
	}
}
