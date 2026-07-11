package evaluation

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// PricingSchemaVersion identifies the operator-owned pricing input contract.
const PricingSchemaVersion = "fgentic.eval.pricing.v1"

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// PricingCatalog is a dated, versioned set of exact provider/model token rates.
type PricingCatalog struct {
	SchemaVersion string        `json:"schema_version"`
	Version       string        `json:"version"`
	EffectiveDate string        `json:"effective_date"`
	Currency      string        `json:"currency"`
	Rates         []PricingRate `json:"rates"`
}

// PricingRate prices supported token types per million tokens.
type PricingRate struct {
	System           string             `json:"system"`
	Model            string             `json:"model"`
	PerMillionTokens map[string]float64 `json:"per_million_tokens"`
}

// CostEstimate is a non-authoritative calculation tied to one catalog version.
type CostEstimate struct {
	Currency       string  `json:"currency"`
	Amount         float64 `json:"amount"`
	CatalogVersion string  `json:"catalog_version"`
	EffectiveDate  string  `json:"effective_date"`
	Model          string  `json:"model"`
}

// DecodePricingCatalog strictly decodes and validates an operator catalog.
func DecodePricingCatalog(input io.Reader) (*PricingCatalog, error) {
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	var catalog PricingCatalog
	if err := decoder.Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decode pricing catalog: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	if err := catalog.Validate(); err != nil {
		return nil, err
	}
	return &catalog, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode pricing catalog: multiple JSON values")
		}
		return fmt.Errorf("decode pricing catalog trailer: %w", err)
	}
	return nil
}

// Validate enforces catalog identity, date, currency, and complete base rates.
func (c *PricingCatalog) Validate() error {
	if c.SchemaVersion != PricingSchemaVersion {
		return fmt.Errorf("pricing schema_version = %q, want %q", c.SchemaVersion, PricingSchemaVersion)
	}
	if strings.TrimSpace(c.Version) == "" {
		return fmt.Errorf("pricing catalog version is required")
	}
	if _, err := time.Parse(time.DateOnly, c.EffectiveDate); err != nil {
		return fmt.Errorf("pricing catalog effective_date must be YYYY-MM-DD: %w", err)
	}
	if !currencyPattern.MatchString(c.Currency) {
		return fmt.Errorf("pricing catalog currency must be a three-letter uppercase code")
	}
	seen := make(map[string]struct{}, len(c.Rates))
	for index, rate := range c.Rates {
		if rate.System == "" || rate.Model == "" {
			return fmt.Errorf("pricing rate %d system and model are required", index)
		}
		key := rate.System + "\x00" + rate.Model
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("duplicate pricing rate for %s/%s", rate.System, rate.Model)
		}
		seen[key] = struct{}{}
		for tokenType, value := range rate.PerMillionTokens {
			switch tokenType {
			case "input", "output", "input_cache_read", "input_cache_write":
			default:
				return fmt.Errorf("pricing rate %s/%s has unsupported token type %q", rate.System, rate.Model, tokenType)
			}
			if value < 0 {
				return fmt.Errorf("pricing rate %s/%s token type %s is negative", rate.System, rate.Model, tokenType)
			}
		}
		if _, found := rate.PerMillionTokens["input"]; !found {
			return fmt.Errorf("pricing rate %s/%s has no input rate", rate.System, rate.Model)
		}
		if _, found := rate.PerMillionTokens["output"]; !found {
			return fmt.Errorf("pricing rate %s/%s has no output rate", rate.System, rate.Model)
		}
	}
	return nil
}

// Estimate prices observed tokens only when an exact catalog entry exists.
func (c *PricingCatalog) Estimate(usage UsageDelta) (*CostEstimate, error) {
	if c == nil {
		return nil, nil
	}
	model := usage.Identity.ResponseModel
	if model == "" || model == "unknown" {
		model = usage.Identity.RequestModel
	}
	var selected *PricingRate
	for index := range c.Rates {
		rate := &c.Rates[index]
		if rate.System == usage.Identity.System && rate.Model == model {
			selected = rate
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("pricing catalog %q has no exact rate for %s/%s", c.Version, usage.Identity.System, model)
	}
	amount := 0.0
	for tokenType, delta := range usage.TokenTypes {
		rate, found := selected.PerMillionTokens[tokenType]
		if !found && delta.Tokens > 0 {
			return nil, fmt.Errorf("pricing catalog %q has no %s rate for %s/%s", c.Version, tokenType, usage.Identity.System, model)
		}
		amount += float64(delta.Tokens) * rate / 1_000_000
	}
	return &CostEstimate{
		Currency:       c.Currency,
		Amount:         amount,
		CatalogVersion: c.Version,
		EffectiveDate:  c.EffectiveDate,
		Model:          model,
	}, nil
}
