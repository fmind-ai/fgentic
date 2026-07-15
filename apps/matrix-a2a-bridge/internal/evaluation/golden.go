package evaluation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// GoldenSchemaVersion identifies the checked-in reference-agent regression contract.
const GoldenSchemaVersion = "fgentic.eval.golden.v1"

const maxGoldenAnswerBytes = 1 << 20

var (
	sha256Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	includePattern = regexp.MustCompile(`\{\{include "zoo/([a-z0-9-]+)"\}\}`)
)

// GoldenSuite pins one deterministic task and effective source contract per shipped agent.
type GoldenSuite struct {
	SchemaVersion string       `json:"schema_version"`
	Cases         []GoldenCase `json:"cases"`
}

// GoldenCase binds an existing evaluation scenario to the exact deterministic demo result.
type GoldenCase struct {
	ScenarioID          string `json:"scenario_id"`
	Agent               Agent  `json:"agent"`
	Prompt              string `json:"prompt"`
	ExpectedAnswer      string `json:"expected_answer"`
	AgentContractSHA256 string `json:"agent_contract_sha256"`
}

// GoldenAnswers contains the deterministic stub output captured for each golden task.
type GoldenAnswers struct {
	Answers []GoldenAnswer `json:"answers"`
}

// GoldenAnswer binds one captured output to its fixed scenario.
type GoldenAnswer struct {
	ScenarioID string `json:"scenario_id"`
	Answer     string `json:"answer"`
}

// GoldenResult describes one verified checked-in agent contract.
type GoldenResult struct {
	ScenarioID          string
	Agent               Agent
	AgentContractSHA256 string
}

type kubeDocument struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Data map[string]string `yaml:"data"`
	Spec map[string]any    `yaml:"spec"`
}

type agentContract struct {
	Spec            map[string]any    `json:"spec"`
	PromptFragments map[string]string `json:"prompt_fragments"`
}

// DecodeGoldenSuite strictly decodes the versioned fixture.
func DecodeGoldenSuite(input io.Reader) (GoldenSuite, error) {
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	var suite GoldenSuite
	if err := decoder.Decode(&suite); err != nil {
		return GoldenSuite{}, fmt.Errorf("decode golden suite: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return GoldenSuite{}, fmt.Errorf("decode golden suite: multiple JSON values")
		}
		return GoldenSuite{}, fmt.Errorf("decode golden suite trailer: %w", err)
	}
	return suite, nil
}

// DecodeGoldenAnswers strictly decodes the bounded output captured from the demo stub.
func DecodeGoldenAnswers(input io.Reader) (GoldenAnswers, error) {
	limited := io.LimitReader(input, maxGoldenAnswerBytes+1)
	encoded, err := io.ReadAll(limited)
	if err != nil {
		return GoldenAnswers{}, fmt.Errorf("read deterministic demo answers: %w", err)
	}
	if len(encoded) > maxGoldenAnswerBytes {
		return GoldenAnswers{}, fmt.Errorf("deterministic demo answers exceed %d bytes", maxGoldenAnswerBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	var answers GoldenAnswers
	if err := decoder.Decode(&answers); err != nil {
		return GoldenAnswers{}, fmt.Errorf("decode deterministic demo answers: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return GoldenAnswers{}, fmt.Errorf("decode deterministic demo answers: multiple JSON values")
		}
		return GoldenAnswers{}, fmt.Errorf("decode deterministic demo answers trailer: %w", err)
	}
	return answers, nil
}

// VerifyGoldenSuite binds the fixture to the shipped Agent sources and deployed demo response.
func VerifyGoldenSuite(
	suite GoldenSuite,
	scenarios []Scenario,
	agentManifests io.Reader,
	promptManifest io.Reader,
	actualAnswers GoldenAnswers,
) ([]GoldenResult, error) {
	if err := validateGoldenSuite(suite, scenarios); err != nil {
		return nil, err
	}
	agents, err := decodeDocuments(agentManifests, "Agent")
	if err != nil {
		return nil, fmt.Errorf("decode agent manifests: %w", err)
	}
	prompts, err := namedConfigMapData(promptManifest, "agent-zoo-prompts")
	if err != nil {
		return nil, fmt.Errorf("decode prompt manifest: %w", err)
	}
	if len(agents) != len(suite.Cases) {
		return nil, fmt.Errorf("shipped Agent count = %d, golden cases = %d", len(agents), len(suite.Cases))
	}
	answersByScenario, err := indexGoldenAnswers(actualAnswers, suite)
	if err != nil {
		return nil, err
	}

	results := make([]GoldenResult, 0, len(suite.Cases))
	for _, golden := range suite.Cases {
		agent, found := agents[string(golden.Agent)]
		if !found {
			return nil, fmt.Errorf("golden agent %q is absent from shipped manifests", golden.Agent)
		}
		digest, err := contractDigest(agent, prompts)
		if err != nil {
			return nil, fmt.Errorf("golden agent %q: %w", golden.Agent, err)
		}
		if digest != golden.AgentContractSHA256 {
			return nil, fmt.Errorf(
				"golden agent %q contract sha256 = %s, fixture requires %s",
				golden.Agent,
				digest,
				golden.AgentContractSHA256,
			)
		}
		score, err := ScoreAnswer(
			answersByScenario[golden.ScenarioID],
			Rubric{Kind: RubricExact, Expected: []string{golden.ExpectedAnswer}},
		)
		if err != nil {
			return nil, fmt.Errorf("score golden scenario %q: %w", golden.ScenarioID, err)
		}
		if score.Verdict != VerdictPass {
			return nil, fmt.Errorf("golden scenario %q failed: %s", golden.ScenarioID, score.Reason)
		}
		results = append(results, GoldenResult{
			ScenarioID:          golden.ScenarioID,
			Agent:               golden.Agent,
			AgentContractSHA256: digest,
		})
	}
	return results, nil
}

func indexGoldenAnswers(answers GoldenAnswers, suite GoldenSuite) (map[string]string, error) {
	if len(answers.Answers) != len(suite.Cases) {
		return nil, fmt.Errorf("demo answer count = %d, golden cases = %d", len(answers.Answers), len(suite.Cases))
	}
	indexed := make(map[string]string, len(answers.Answers))
	for _, answer := range answers.Answers {
		if _, duplicate := indexed[answer.ScenarioID]; duplicate {
			return nil, fmt.Errorf("duplicate demo answer for scenario %q", answer.ScenarioID)
		}
		if strings.TrimSpace(answer.Answer) == "" {
			return nil, fmt.Errorf("demo answer for scenario %q must be non-empty", answer.ScenarioID)
		}
		indexed[answer.ScenarioID] = answer.Answer
	}
	for _, golden := range suite.Cases {
		if _, found := indexed[golden.ScenarioID]; !found {
			return nil, fmt.Errorf("demo answer for golden scenario %q is missing", golden.ScenarioID)
		}
	}
	return indexed, nil
}

func validateGoldenSuite(suite GoldenSuite, scenarios []Scenario) error {
	if suite.SchemaVersion != GoldenSchemaVersion {
		return fmt.Errorf("golden schema_version = %q, want %q", suite.SchemaVersion, GoldenSchemaVersion)
	}
	if err := ValidateScenarios(scenarios); err != nil {
		return err
	}
	scenarioByID := make(map[string]Scenario, len(scenarios))
	for _, scenario := range scenarios {
		scenarioByID[scenario.ID] = scenario
	}
	seenAgents := make(map[Agent]struct{}, len(suite.Cases))
	seenScenarios := make(map[string]struct{}, len(suite.Cases))
	for _, golden := range suite.Cases {
		scenario, found := scenarioByID[golden.ScenarioID]
		if !found {
			return fmt.Errorf("golden scenario %q is not in the fixed evaluation suite", golden.ScenarioID)
		}
		if _, duplicate := seenScenarios[golden.ScenarioID]; duplicate {
			return fmt.Errorf("duplicate golden scenario %q", golden.ScenarioID)
		}
		seenScenarios[golden.ScenarioID] = struct{}{}
		if _, duplicate := seenAgents[golden.Agent]; duplicate {
			return fmt.Errorf("duplicate golden agent %q", golden.Agent)
		}
		seenAgents[golden.Agent] = struct{}{}
		if scenario.Agent != golden.Agent || scenario.Prompt != golden.Prompt {
			return fmt.Errorf("golden scenario %q agent or prompt drifted from the fixed suite", golden.ScenarioID)
		}
		if strings.TrimSpace(golden.ExpectedAnswer) == "" {
			return fmt.Errorf("golden scenario %q expected answer is required", golden.ScenarioID)
		}
		if !sha256Pattern.MatchString(golden.AgentContractSHA256) {
			return fmt.Errorf("golden scenario %q agent contract sha256 is invalid", golden.ScenarioID)
		}
	}
	for _, agent := range []Agent{AgentPlatformHelper, AgentDocsQA, AgentScribe} {
		if _, found := seenAgents[agent]; !found {
			return fmt.Errorf("golden suite has no case for shipped agent %q", agent)
		}
	}
	if len(suite.Cases) != len(seenAgents) {
		return fmt.Errorf("golden suite must contain exactly one case per shipped agent")
	}
	return nil
}

func decodeDocuments(input io.Reader, kind string) (map[string]kubeDocument, error) {
	decoder := yaml.NewDecoder(input)
	documents := make(map[string]kubeDocument)
	for {
		var document kubeDocument
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if document.Kind != kind {
			continue
		}
		if document.Metadata.Name == "" {
			return nil, fmt.Errorf("%s document has no metadata.name", kind)
		}
		if _, duplicate := documents[document.Metadata.Name]; duplicate {
			return nil, fmt.Errorf("duplicate %s %q", kind, document.Metadata.Name)
		}
		documents[document.Metadata.Name] = document
	}
	return documents, nil
}

func namedConfigMapData(input io.Reader, name string) (map[string]string, error) {
	documents, err := decodeDocuments(input, "ConfigMap")
	if err != nil {
		return nil, err
	}
	document, found := documents[name]
	if !found {
		return nil, fmt.Errorf("ConfigMap %q is absent", name)
	}
	return document.Data, nil
}

func contractDigest(agent kubeDocument, prompts map[string]string) (string, error) {
	systemMessage, found := nestedString(agent.Spec, "declarative", "systemMessage")
	if !found {
		return "", fmt.Errorf("Agent has no spec.declarative.systemMessage")
	}
	fragments := make(map[string]string)
	for _, match := range includePattern.FindAllStringSubmatch(systemMessage, -1) {
		key := match[1]
		value, found := prompts[key]
		if !found {
			return "", fmt.Errorf("Agent references missing prompt fragment %q", key)
		}
		fragments[key] = value
	}
	if len(fragments) == 0 {
		return "", fmt.Errorf("Agent references no prompt fragments")
	}
	encoded, err := json.Marshal(agentContract{Spec: agent.Spec, PromptFragments: fragments})
	if err != nil {
		return "", fmt.Errorf("marshal Agent contract: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func nestedString(input map[string]any, path ...string) (string, bool) {
	var current any = input
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = object[key]
		if !ok {
			return "", false
		}
	}
	value, ok := current.(string)
	return value, ok
}
