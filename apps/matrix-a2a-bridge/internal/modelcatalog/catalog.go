// Package modelcatalog parses the governed model inventory shared by validation and evaluation.
package modelcatalog

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// SchemaVersion is the only catalog schema understood by this build.
	SchemaVersion = 1
	// PricingSchemaVersion is the optional operator pricing overlay a model may reference.
	PricingSchemaVersion = "fgentic.eval.pricing.v1"
)

// Residency identifies the model-serving location controlled by policy.
type Residency string

const (
	// ResidencySelfHosted keeps model serving inside the selected cluster.
	ResidencySelfHosted Residency = "self-hosted"
	// ResidencyEU sends model traffic to an operator-reviewed EU service boundary.
	ResidencyEU Residency = "eu"
	// ResidencyGlobal permits provider processing outside a constrained region.
	ResidencyGlobal Residency = "global"
)

// Classification is the highest room-data class a model is approved to serve.
type Classification string

const (
	// ClassificationPublic admits only public room data.
	ClassificationPublic Classification = "public"
	// ClassificationApprovedNonPublic admits explicitly approved non-public room data.
	ClassificationApprovedNonPublic Classification = "approved_non_public"
	// ClassificationRestricted admits restricted room data.
	ClassificationRestricted Classification = "restricted"
	// ClassificationRegulated admits regulated room data.
	ClassificationRegulated Classification = "regulated"
)

// Capability is a model API surface admitted by the inventory.
type Capability string

const (
	// CapabilityChat serves conversational completion requests.
	CapabilityChat Capability = "chat"
	// CapabilityEmbeddings creates vector embeddings.
	CapabilityEmbeddings Capability = "embeddings"
	// CapabilityRerank scores and orders candidate results.
	CapabilityRerank Capability = "rerank"
)

var profilePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Catalog is the versioned, declarative model inventory.
type Catalog struct {
	SchemaVersion int     `yaml:"schemaVersion"`
	Models        []Model `yaml:"models"`
}

// Model is one exact profile, metric identity, and policy tuple.
type Model struct {
	Profile               string         `yaml:"profile"`
	GenAISystem           string         `yaml:"genAiSystem"`
	Name                  string         `yaml:"model"`
	Residency             Residency      `yaml:"residency"`
	AllowedClassification Classification `yaml:"allowedClassification"`
	Capabilities          []Capability   `yaml:"capabilities"`
	CostRef               string         `yaml:"costRef,omitempty"`
}

// Decode strictly parses and validates one catalog document.
func Decode(input io.Reader) (*Catalog, error) {
	decoder := yaml.NewDecoder(input)
	decoder.KnownFields(true)
	var catalog Catalog
	if err := decoder.Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decode model catalog: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode model catalog: multiple YAML documents")
		}
		return nil, fmt.Errorf("decode model catalog trailer: %w", err)
	}
	if err := catalog.Validate(); err != nil {
		return nil, err
	}
	return &catalog, nil
}

// Validate enforces schema, enums, exact identities, and uniqueness.
func (c *Catalog) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("model catalog schemaVersion = %d, want %d", c.SchemaVersion, SchemaVersion)
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("model catalog requires at least one model")
	}
	identities := make(map[string]struct{}, len(c.Models))
	selections := make(map[string]struct{}, len(c.Models))
	for index := range c.Models {
		model := &c.Models[index]
		if err := model.validate(); err != nil {
			return fmt.Errorf("model %d: %w", index, err)
		}
		identity := model.GenAISystem + "\x00" + model.Name
		if _, duplicate := identities[identity]; duplicate {
			return fmt.Errorf("duplicate model identity %s/%s", model.GenAISystem, model.Name)
		}
		identities[identity] = struct{}{}
		selection := model.Profile + "\x00" + model.Name
		if _, duplicate := selections[selection]; duplicate {
			return fmt.Errorf("duplicate profile/model selection %s/%s", model.Profile, model.Name)
		}
		selections[selection] = struct{}{}
	}
	return nil
}

func (m *Model) validate() error {
	if !profilePattern.MatchString(m.Profile) {
		return fmt.Errorf("profile %q must be a lowercase DNS-style label", m.Profile)
	}
	if strings.TrimSpace(m.GenAISystem) == "" || strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("genAiSystem and model are required")
	}
	switch m.Residency {
	case ResidencySelfHosted, ResidencyEU, ResidencyGlobal:
	default:
		return fmt.Errorf("residency %q is not supported", m.Residency)
	}
	switch m.AllowedClassification {
	case ClassificationPublic, ClassificationApprovedNonPublic, ClassificationRestricted, ClassificationRegulated:
	default:
		return fmt.Errorf("allowedClassification %q is not supported", m.AllowedClassification)
	}
	if len(m.Capabilities) == 0 {
		return fmt.Errorf("capabilities must not be empty")
	}
	seen := make(map[Capability]struct{}, len(m.Capabilities))
	for _, capability := range m.Capabilities {
		switch capability {
		case CapabilityChat, CapabilityEmbeddings, CapabilityRerank:
		default:
			return fmt.Errorf("capability %q is not supported", capability)
		}
		if _, duplicate := seen[capability]; duplicate {
			return fmt.Errorf("duplicate capability %q", capability)
		}
		seen[capability] = struct{}{}
	}
	if m.CostRef != "" && m.CostRef != PricingSchemaVersion {
		return fmt.Errorf("costRef %q must name %q", m.CostRef, PricingSchemaVersion)
	}
	return nil
}

// ResolveProfile returns the exact governed entry for a deployed profile/model selection.
func (c *Catalog) ResolveProfile(profile, model string) (Model, error) {
	for _, candidate := range c.Models {
		if candidate.Profile == profile && candidate.Name == model {
			return candidate, nil
		}
	}
	return Model{}, fmt.Errorf("model catalog has no entry for profile/model %s/%s", profile, model)
}

// Supports reports whether this exact model declares a capability.
func (m Model) Supports(capability Capability) bool {
	for _, candidate := range m.Capabilities {
		if candidate == capability {
			return true
		}
	}
	return false
}

// ParseClassification converts an external policy value into the closed classification enum.
func ParseClassification(value string) (Classification, error) {
	classification := Classification(value)
	switch classification {
	case ClassificationPublic, ClassificationApprovedNonPublic, ClassificationRestricted, ClassificationRegulated:
		return classification, nil
	default:
		return "", fmt.Errorf("classification %q is not supported", value)
	}
}

// ClassificationOrMostRestrictive parses a value fail-closed: an empty or unknown value collapses
// to the most-restrictive class so untrusted or missing signals are treated as the most sensitive.
// This mirrors the bridge's request-header default and the gateway CEL's fail-closed header ladder.
func ClassificationOrMostRestrictive(value string) Classification {
	classification, err := ParseClassification(value)
	if err != nil {
		return ClassificationRegulated
	}
	return classification
}

// Rank orders the closed classification enum from least (public) to most (regulated) sensitive.
// An unrecognized value ranks as the most sensitive so any drift denies rather than leaks. The
// deployed agentgateway CEL encodes this exact ladder; scripts/test-model-residency.sh binds them.
func (c Classification) Rank() int {
	switch c {
	case ClassificationPublic:
		return 0
	case ClassificationApprovedNonPublic:
		return 1
	case ClassificationRestricted:
		return 2
	case ClassificationRegulated:
		return 3
	default:
		return 3
	}
}

// Admits reports whether a model whose ceiling is allowedClassification may serve room content of
// class roomClass. Serving is permitted only when the room's sensitivity does not exceed the
// model's approved ceiling (roomClass.Rank() <= ceiling.Rank()). This is the residency decision
// enforced fail-closed at the agentgateway egress chokepoint before any model egress occurs.
func (m Model) Admits(roomClass Classification) bool {
	return roomClass.Rank() <= m.AllowedClassification.Rank()
}
