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

// AgentGoldenSchemaVersion identifies the per-Agent deterministic regression contract.
const AgentGoldenSchemaVersion = "fgentic.agent.eval.v1"

const (
	maxGoldenAnswerBytes = 1 << 20
	maxGoldenDiffRunes   = 4096
)

var (
	sha256Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	includePattern = regexp.MustCompile(`\{\{include "zoo/([a-z0-9-]+)"\}\}`)
)

// AgentGoldenSuite pins deterministic tasks and the effective source contract for one Agent.
type AgentGoldenSuite struct {
	SchemaVersion       string     `json:"schema_version"`
	Agent               Agent      `json:"agent"`
	AgentContractSHA256 string     `json:"agent_contract_sha256"`
	Scenarios           []Scenario `json:"scenarios"`
}

// GoldenAnswers contains deterministic stub output captured for every checked-in task.
type GoldenAnswers struct {
	Answers []GoldenAnswer `json:"answers"`
}

// GoldenAnswer binds one captured output to its fixed scenario.
type GoldenAnswer struct {
	ScenarioID string `json:"scenario_id"`
	Answer     string `json:"answer"`
}

// GoldenResult describes one verified Agent scenario and source contract.
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

// DecodeAgentGoldenSuite strictly decodes one versioned per-Agent fixture.
func DecodeAgentGoldenSuite(input io.Reader) (AgentGoldenSuite, error) {
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	var suite AgentGoldenSuite
	if err := decoder.Decode(&suite); err != nil {
		return AgentGoldenSuite{}, fmt.Errorf("decode Agent golden suite: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return AgentGoldenSuite{}, fmt.Errorf("decode Agent golden suite: multiple JSON values")
		}
		return AgentGoldenSuite{}, fmt.Errorf("decode Agent golden suite trailer: %w", err)
	}
	return suite, nil
}

// DecodeGoldenAnswers strictly decodes bounded output captured from the demo stub.
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

// VerifyAgentGoldenSuites binds every fixture to one rendered Agent and the demo response.
func VerifyAgentGoldenSuites(
	suites []AgentGoldenSuite,
	agentManifests io.Reader,
	promptManifest io.Reader,
	actualAnswers GoldenAnswers,
) ([]GoldenResult, error) {
	return verifyAgentGoldenSuites(suites, agentManifests, promptManifest, actualAnswers, true)
}

// VerifyAgentGoldenSuite runs one Agent through the same deterministic assertions as the complete CI gate.
func VerifyAgentGoldenSuite(
	suite AgentGoldenSuite,
	agentManifests io.Reader,
	promptManifest io.Reader,
	actualAnswers GoldenAnswers,
) ([]GoldenResult, error) {
	return verifyAgentGoldenSuites(
		[]AgentGoldenSuite{suite},
		agentManifests,
		promptManifest,
		actualAnswers,
		false,
	)
}

func verifyAgentGoldenSuites(
	suites []AgentGoldenSuite,
	agentManifests io.Reader,
	promptManifest io.Reader,
	actualAnswers GoldenAnswers,
	requireCompleteFixtureSet bool,
) ([]GoldenResult, error) {
	agents, err := decodeDocuments(agentManifests, "Agent")
	if err != nil {
		return nil, fmt.Errorf("decode Agent manifests: %w", err)
	}
	prompts, err := namedConfigMapData(promptManifest, "agent-zoo-prompts")
	if err != nil {
		return nil, fmt.Errorf("decode Agent prompts: %w", err)
	}
	if requireCompleteFixtureSet && len(suites) != len(agents) {
		return nil, fmt.Errorf("rendered Agent count = %d, golden fixture count = %d", len(agents), len(suites))
	}

	answersByScenario, err := indexGoldenAnswers(actualAnswers)
	if err != nil {
		return nil, err
	}
	seenAgents := make(map[Agent]struct{}, len(suites))
	seenScenarios := make(map[string]struct{})
	results := make([]GoldenResult, 0, len(actualAnswers.Answers))
	for _, suite := range suites {
		if err := validateAgentGoldenSuite(suite, seenAgents, seenScenarios); err != nil {
			return nil, err
		}
		agent, found := agents[string(suite.Agent)]
		if !found {
			return nil, fmt.Errorf("golden Agent %q is absent from rendered manifests", suite.Agent)
		}
		digest, digestErr := contractDigest(agent, prompts)
		if digestErr != nil {
			return nil, fmt.Errorf("golden Agent %q: %w", suite.Agent, digestErr)
		}
		if digest != suite.AgentContractSHA256 {
			return nil, fmt.Errorf(
				"golden Agent %q contract sha256 = %s, fixture requires %s",
				suite.Agent,
				digest,
				suite.AgentContractSHA256,
			)
		}
		for _, scenario := range suite.Scenarios {
			answer, found := answersByScenario[scenario.ID]
			if !found {
				return nil, fmt.Errorf("deterministic demo answer for golden scenario %q is missing", scenario.ID)
			}
			score, scoreErr := ScoreAnswer(answer, scenario.Rubric)
			if scoreErr != nil {
				return nil, fmt.Errorf("score golden scenario %q: %w", scenario.ID, scoreErr)
			}
			if score.Verdict != VerdictPass {
				return nil, fmt.Errorf("golden scenario %q failed: %s", scenario.ID, goldenFailureDetail(answer, scenario.Rubric, score.Reason))
			}
			results = append(results, GoldenResult{
				ScenarioID:          scenario.ID,
				Agent:               suite.Agent,
				AgentContractSHA256: digest,
			})
		}
	}
	if requireCompleteFixtureSet {
		for name := range agents {
			if _, found := seenAgents[Agent(name)]; !found {
				return nil, fmt.Errorf("rendered Agent %q has no evals/%s/golden.json fixture", name, name)
			}
		}
	}
	if len(answersByScenario) != len(seenScenarios) {
		return nil, fmt.Errorf("deterministic demo answer count = %d, golden scenario count = %d", len(answersByScenario), len(seenScenarios))
	}
	return results, nil
}

func goldenFailureDetail(answer string, rubric Rubric, reason string) string {
	if rubric.Kind == RubricExact {
		expected := boundedGoldenDiffValue(rubric.Expected[0])
		actual := boundedGoldenDiffValue(answer)
		return fmt.Sprintf(
			"exact answer mismatch\n--- expected\n+++ actual\n-%s\n+%s",
			strings.ReplaceAll(expected, "\n", "\n-"),
			strings.ReplaceAll(actual, "\n", "\n+"),
		)
	}
	return fmt.Sprintf("%s\nactual: %q", reason, boundedGoldenDiffValue(answer))
}

func boundedGoldenDiffValue(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= maxGoldenDiffRunes {
		return string(runes)
	}
	return string(runes[:maxGoldenDiffRunes]) + "\n... output truncated"
}

// AgentContractDigest returns the stable effective contract hash for one rendered Agent.
func AgentContractDigest(agentName string, agentManifests io.Reader, promptManifest io.Reader) (string, error) {
	agents, err := decodeDocuments(agentManifests, "Agent")
	if err != nil {
		return "", fmt.Errorf("decode Agent manifests: %w", err)
	}
	agent, found := agents[agentName]
	if !found {
		return "", fmt.Errorf("Agent %q is absent from rendered manifests", agentName)
	}
	prompts, err := namedConfigMapData(promptManifest, "agent-zoo-prompts")
	if err != nil {
		return "", fmt.Errorf("decode Agent prompts: %w", err)
	}
	digest, err := contractDigest(agent, prompts)
	if err != nil {
		return "", fmt.Errorf("Agent %q: %w", agentName, err)
	}
	return digest, nil
}

func indexGoldenAnswers(answers GoldenAnswers) (map[string]string, error) {
	indexed := make(map[string]string, len(answers.Answers))
	for _, answer := range answers.Answers {
		if _, duplicate := indexed[answer.ScenarioID]; duplicate {
			return nil, fmt.Errorf("duplicate deterministic demo answer for scenario %q", answer.ScenarioID)
		}
		if strings.TrimSpace(answer.Answer) == "" {
			return nil, fmt.Errorf("deterministic demo answer for scenario %q must be non-empty", answer.ScenarioID)
		}
		indexed[answer.ScenarioID] = answer.Answer
	}
	return indexed, nil
}

func validateAgentGoldenSuite(
	suite AgentGoldenSuite,
	seenAgents map[Agent]struct{},
	seenScenarios map[string]struct{},
) error {
	if suite.SchemaVersion != AgentGoldenSchemaVersion {
		return fmt.Errorf("golden Agent %q schema_version = %q, want %q", suite.Agent, suite.SchemaVersion, AgentGoldenSchemaVersion)
	}
	if strings.TrimSpace(string(suite.Agent)) == "" {
		return fmt.Errorf("golden fixture Agent is required")
	}
	if _, duplicate := seenAgents[suite.Agent]; duplicate {
		return fmt.Errorf("duplicate golden fixture for Agent %q", suite.Agent)
	}
	seenAgents[suite.Agent] = struct{}{}
	if !sha256Pattern.MatchString(suite.AgentContractSHA256) {
		return fmt.Errorf("golden Agent %q contract sha256 is invalid", suite.Agent)
	}
	if len(suite.Scenarios) == 0 {
		return fmt.Errorf("golden Agent %q has no scenarios", suite.Agent)
	}
	for _, scenario := range suite.Scenarios {
		if strings.TrimSpace(scenario.ID) == "" || strings.TrimSpace(scenario.Prompt) == "" {
			return fmt.Errorf("golden Agent %q scenario id and prompt must be non-empty", suite.Agent)
		}
		if scenario.Agent != suite.Agent {
			return fmt.Errorf("golden scenario %q names Agent %q, fixture names %q", scenario.ID, scenario.Agent, suite.Agent)
		}
		if _, duplicate := seenScenarios[scenario.ID]; duplicate {
			return fmt.Errorf("duplicate golden scenario %q", scenario.ID)
		}
		seenScenarios[scenario.ID] = struct{}{}
		if scenario.Rubric.Kind == RubricOptionalLLMJudge {
			return fmt.Errorf("golden scenario %q must use a deterministic rubric", scenario.ID)
		}
		if err := validateRubric(scenario.ID, scenario.Rubric); err != nil {
			return err
		}
	}
	return nil
}

func validateRubric(scenarioID string, rubric Rubric) error {
	for _, forbidden := range rubric.Forbidden {
		if strings.TrimSpace(forbidden) == "" {
			return fmt.Errorf("scenario %q forbidden values must be non-blank", scenarioID)
		}
	}
	switch rubric.Kind {
	case RubricExact:
		if len(rubric.Expected) != 1 || strings.TrimSpace(rubric.Expected[0]) == "" {
			return fmt.Errorf("scenario %q exact rubric needs one non-blank expected value", scenarioID)
		}
	case RubricContains:
		if len(rubric.Expected) == 0 {
			return fmt.Errorf("scenario %q contains rubric needs non-blank expected values", scenarioID)
		}
		for _, expected := range rubric.Expected {
			if strings.TrimSpace(expected) == "" {
				return fmt.Errorf("scenario %q contains rubric needs non-blank expected values", scenarioID)
			}
		}
	case RubricRegex:
		if strings.TrimSpace(rubric.Pattern) == "" {
			return fmt.Errorf("scenario %q regex rubric needs a non-blank pattern", scenarioID)
		}
		if _, err := regexp.Compile(rubric.Pattern); err != nil {
			return fmt.Errorf("scenario %q regex rubric: %w", scenarioID, err)
		}
	default:
		return fmt.Errorf("scenario %q has non-deterministic or unknown rubric %q", scenarioID, rubric.Kind)
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
