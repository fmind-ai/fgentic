package usagereceipt

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// ExtensionURI identifies the versioned receipt in A2A activation and metadata fields.
const ExtensionURI = "https://fgentic.fmind.ai/a2a/extensions/usage-receipt/v1"

var validTaskStates = map[a2a.TaskState]bool{
	a2a.TaskStateAuthRequired:  true,
	a2a.TaskStateCanceled:      true,
	a2a.TaskStateCompleted:     true,
	a2a.TaskStateFailed:        true,
	a2a.TaskStateInputRequired: true,
	a2a.TaskStateRejected:      true,
	a2a.TaskStateSubmitted:     true,
	a2a.TaskStateWorking:       true,
}

type jsonRPCID string

type requestEvidence struct {
	Method         string    `json:"method"`
	TaskID         string    `json:"taskId,omitempty"`
	RequestHash    string    `json:"requestHash,omitempty"`
	TokensReserved uint64    `json:"tokensReserved,omitempty"`
	JSONRPCID      jsonRPCID `json:"-"`
}

type pendingEvidence struct {
	RequestHash    string `json:"requestHash"`
	TokensReserved uint64 `json:"tokensReserved"`
}

type responseEvidence struct {
	TaskID    string
	ContextID string
	Outcome   string
	Terminal  bool
}

// Processor deterministically attaches receipts after the authenticated upstream returns a
// terminal A2A result. It owns no authorization decision; agentgateway must run it only after the
// exact exported-route JWT, authorization, and reservation policies have accepted the request.
type Processor struct {
	AZP     string
	KeyID   string
	Key     *ecdsa.PrivateKey
	Archive *Archive
	Pending *PendingStore
	Now     func() time.Time
	stateMu sync.Mutex
}

// ParseRequest records only the content-free evidence needed for a later receipt.
func ParseRequest(raw []byte) (requestEvidence, error) {
	requestHash, err := RequestHash(raw)
	if err != nil {
		return requestEvidence{}, err
	}
	var request struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  struct {
			ID      string `json:"id"`
			Message struct {
				Metadata map[string]json.RawMessage `json:"metadata"`
			} `json:"message"`
		} `json:"params"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&request); err != nil {
		return requestEvidence{}, fmt.Errorf("decode A2A request: %w", err)
	}
	if err := expectEOF(decoder); err != nil {
		return requestEvidence{}, fmt.Errorf("decode A2A request: %w", err)
	}
	if request.JSONRPC != "2.0" {
		return requestEvidence{}, fmt.Errorf("A2A request jsonrpc version is invalid")
	}
	jsonRPCID, err := canonicalJSONRPCID(request.ID)
	if err != nil {
		return requestEvidence{}, fmt.Errorf("A2A request id is invalid: %w", err)
	}
	switch request.Method {
	case "SendMessage":
		budgetRaw, ok := request.Params.Message.Metadata["https://fgentic.fmind.ai/a2a/extensions/token-budget/v1"]
		if !ok {
			return requestEvidence{}, fmt.Errorf("SendMessage token-budget metadata is missing")
		}
		var budget struct {
			MaxTokens uint64 `json:"maxTokens"`
		}
		budgetDecoder := json.NewDecoder(bytes.NewReader(budgetRaw))
		budgetDecoder.DisallowUnknownFields()
		if err := budgetDecoder.Decode(&budget); err != nil ||
			expectEOF(budgetDecoder) != nil ||
			!validTokenReservation(budget.MaxTokens) {
			return requestEvidence{}, fmt.Errorf("SendMessage token-budget metadata is invalid")
		}
		return requestEvidence{
			Method: request.Method, RequestHash: requestHash,
			TokensReserved: budget.MaxTokens, JSONRPCID: jsonRPCID,
		}, nil
	case "GetTask":
		if !validIdentifier(request.Params.ID) {
			return requestEvidence{}, fmt.Errorf("GetTask id is invalid")
		}
		return requestEvidence{
			Method: request.Method, TaskID: request.Params.ID, JSONRPCID: jsonRPCID,
		}, nil
	default:
		return requestEvidence{}, nil
	}
}

// RequestHash returns the RFC 8785 hash bound into a usage receipt. Integers outside the
// interoperable IEEE-754 safe range are rejected before canonicalization so distinct accepted
// requests cannot collapse onto the same signed hash.
func RequestHash(raw []byte) (string, error) {
	if err := validateJCSInput(raw); err != nil {
		return "", err
	}
	canonical, err := canonicalizeIJSON(raw)
	if err != nil {
		return "", fmt.Errorf("A2A request is not valid canonicalizable I-JSON")
	}
	digest := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func validateJCSInput(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("decode A2A request for canonicalization: %w", err)
	}
	if err := expectEOF(decoder); err != nil {
		return fmt.Errorf("decode A2A request for canonicalization: %w", err)
	}
	if err := validateJCSNumbers(value); err != nil {
		return fmt.Errorf("validate A2A request for canonicalization: %w", err)
	}
	return nil
}

func validateJCSNumbers(value any) error {
	switch typed := value.(type) {
	case json.Number:
		rawRational, ok := new(big.Rat).SetString(string(typed))
		if !ok {
			return fmt.Errorf("invalid JSON number")
		}
		canonical, err := canonicalizeIJSON([]byte(typed))
		if err != nil {
			return fmt.Errorf("canonicalize JSON number: %w", err)
		}
		canonicalRational, ok := new(big.Rat).SetString(string(canonical))
		if !ok {
			return fmt.Errorf("invalid canonical JSON number")
		}
		if rawRational.Cmp(canonicalRational) != 0 {
			return fmt.Errorf("JSON number loses precision under RFC 8785")
		}
	case []any:
		for _, item := range typed {
			if err := validateJCSNumbers(item); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, item := range typed {
			if err := validateJCSNumbers(item); err != nil {
				return err
			}
		}
	}
	return nil
}

func canonicalJSONRPCID(raw json.RawMessage) (jsonRPCID, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("missing JSON-RPC id")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", fmt.Errorf("decode JSON-RPC id: %w", err)
	}
	if err := expectEOF(decoder); err != nil {
		return "", fmt.Errorf("decode JSON-RPC id: %w", err)
	}
	switch value.(type) {
	case nil, string, json.Number:
	default:
		return "", fmt.Errorf("JSON-RPC id must be a string, number, or null")
	}
	if err := validateJCSNumbers(value); err != nil {
		return "", fmt.Errorf("validate JSON-RPC id: %w", err)
	}
	canonical, err := canonicalizeIJSON(raw)
	if err != nil {
		return "", fmt.Errorf("canonicalize JSON-RPC id: %w", err)
	}
	return jsonRPCID(canonical), nil
}

// TransformResponse signs and injects a receipt when raw contains a terminal result. Nonterminal,
// JSON-RPC error, and unrelated-method responses pass through byte-for-byte. A result-bearing
// response with an invalid envelope fails closed so the caller cannot accept success without the
// required evidence.
func (p *Processor) TransformResponse(request requestEvidence, raw []byte) ([]byte, bool, error) {
	if request.Method != "SendMessage" && request.Method != "GetTask" {
		return raw, false, nil
	}
	if err := p.validate(); err != nil {
		return nil, false, err
	}
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   json.RawMessage `json:"error"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return raw, false, nil
	}
	if len(envelope.Result) == 0 {
		return raw, false, nil
	}
	if trimmed := bytes.TrimSpace(envelope.Error); len(trimmed) > 0 &&
		!bytes.Equal(trimmed, []byte("null")) {
		return nil, false, fmt.Errorf("A2A response contains both result and error")
	}
	if _, err := canonicalizeIJSON(raw); err != nil {
		return nil, false, fmt.Errorf("result-bearing A2A response is not valid canonicalizable I-JSON")
	}
	if envelope.JSONRPC != "2.0" {
		return nil, false, fmt.Errorf("A2A response jsonrpc version is invalid")
	}
	responseID, err := canonicalJSONRPCID(envelope.ID)
	if err != nil {
		return nil, false, fmt.Errorf("A2A response id is invalid: %w", err)
	}
	if responseID != request.JSONRPCID {
		return nil, false, fmt.Errorf("A2A response id does not match request")
	}
	response, err := validateA2AResult(request.Method, envelope.Result)
	if err != nil {
		return nil, false, err
	}
	result, err := decodeObject(envelope.Result)
	if err != nil {
		return nil, false, fmt.Errorf("decode A2A response result: %w", err)
	}
	if request.Method == "GetTask" {
		if request.TaskID != response.TaskID {
			return nil, false, fmt.Errorf("GetTask response task ID does not match request")
		}
	}

	if !response.Terminal {
		p.stateMu.Lock()
		defer p.stateMu.Unlock()
		archived, found, err := p.Archive.Find(response.TaskID)
		if err != nil {
			return nil, false, err
		}
		if found {
			if err := p.validateArchivedTask(archived, response.TaskID); err != nil {
				return nil, false, err
			}
			if err := p.validatePending(response.TaskID, archived.Receipt); err != nil {
				return nil, false, err
			}
			if err := p.Pending.Delete(response.TaskID); err != nil {
				return nil, false, err
			}
			return nil, false, fmt.Errorf("nonterminal A2A response conflicts with archived terminal task")
		}
		if request.Method == "SendMessage" {
			evidence := pendingEvidence{
				RequestHash: request.RequestHash, TokensReserved: request.TokensReserved,
			}
			if err := p.Pending.Save(response.TaskID, evidence); err != nil {
				return nil, false, err
			}
		}
		return raw, false, nil
	}

	var proposed Signed
	var updated []byte
	var attached bool
	completedAt := p.Now()
	if request.Method == "SendMessage" {
		proposed, updated, attached, err = p.prepareReceipt(
			request,
			response,
			pendingEvidence{
				RequestHash: request.RequestHash, TokensReserved: request.TokensReserved,
			},
			completedAt,
			raw,
			result,
		)
		if err != nil {
			return nil, false, err
		}
	}

	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	archived, found, err := p.Archive.Find(response.TaskID)
	if err != nil {
		return nil, false, err
	}
	if found {
		if err := p.validateArchived(
			archived,
			request,
			response.TaskID,
			response.ContextID,
			response.Outcome,
		); err != nil {
			return nil, false, err
		}
		updated, attached, err := attachReceipt(request.Method, raw, result, archived)
		if err != nil {
			return nil, false, err
		}
		if err := p.validatePending(response.TaskID, archived.Receipt); err != nil {
			return nil, false, err
		}
		if err := p.Pending.Delete(response.TaskID); err != nil {
			return nil, false, err
		}
		return updated, attached, nil
	}
	evidence := pendingEvidence{
		RequestHash: request.RequestHash, TokensReserved: request.TokensReserved,
	}
	if request.Method == "GetTask" {
		evidence, found, err = p.Pending.Load(response.TaskID)
		if err != nil {
			return nil, false, err
		}
		if !found {
			return nil, false, fmt.Errorf("terminal A2A response has no reservation evidence")
		}
	}
	if evidence.RequestHash == "" || evidence.TokensReserved == 0 {
		return nil, false, fmt.Errorf("terminal A2A response has no reservation evidence")
	}
	if request.Method == "GetTask" {
		proposed, updated, attached, err = p.prepareReceipt(
			request,
			response,
			evidence,
			completedAt,
			raw,
			result,
		)
		if err != nil {
			return nil, false, err
		}
	}
	if err := p.validatePending(response.TaskID, proposed.Receipt); err != nil {
		return nil, false, err
	}
	signed, err := p.Archive.AppendUnique(proposed)
	if err != nil {
		return nil, false, err
	}
	if err := p.validateArchived(
		signed,
		request,
		response.TaskID,
		response.ContextID,
		response.Outcome,
	); err != nil {
		return nil, false, err
	}
	if signed != proposed {
		updated, attached, err = attachReceipt(request.Method, raw, result, signed)
		if err != nil {
			return nil, false, err
		}
	}
	if err := p.Pending.Delete(response.TaskID); err != nil {
		return nil, false, err
	}
	return updated, attached, nil
}

func (p *Processor) validateArchived(
	signed Signed,
	request requestEvidence,
	taskID, contextID, outcome string,
) error {
	if err := Verify(signed, &p.Key.PublicKey, p.KeyID); err != nil {
		return fmt.Errorf("validate archived usage receipt: %w", err)
	}
	receipt := signed.Receipt
	if receipt.AZP != p.AZP || receipt.TaskID != taskID || receipt.ContextID != contextID ||
		receipt.Outcome != outcome {
		return fmt.Errorf("archived usage receipt conflicts with terminal task")
	}
	if request.Method == "SendMessage" &&
		(receipt.RequestHash != request.RequestHash || receipt.TokensReserved != request.TokensReserved) {
		return fmt.Errorf("archived usage receipt conflicts with SendMessage reservation")
	}
	return nil
}

func (p *Processor) validateArchivedTask(signed Signed, taskID string) error {
	if err := Verify(signed, &p.Key.PublicKey, p.KeyID); err != nil {
		return fmt.Errorf("validate archived usage receipt: %w", err)
	}
	if signed.Receipt.AZP != p.AZP || signed.Receipt.TaskID != taskID {
		return fmt.Errorf("archived usage receipt conflicts with task")
	}
	return nil
}

func (p *Processor) validatePending(taskID string, receipt Receipt) error {
	evidence, found, err := p.Pending.Load(taskID)
	if err != nil {
		return err
	}
	if found && (evidence.RequestHash != receipt.RequestHash ||
		evidence.TokensReserved != receipt.TokensReserved) {
		return fmt.Errorf("pending usage receipt evidence conflicts with archive")
	}
	return nil
}

func (p *Processor) prepareReceipt(
	request requestEvidence,
	response responseEvidence,
	evidence pendingEvidence,
	completedAt time.Time,
	raw []byte,
	result map[string]any,
) (Signed, []byte, bool, error) {
	receipt, err := New(
		p.AZP,
		response.TaskID,
		response.ContextID,
		evidence.RequestHash,
		evidence.TokensReserved,
		completedAt,
		response.Outcome,
		p.KeyID,
	)
	if err != nil {
		return Signed{}, nil, false, err
	}
	proposed, err := Sign(receipt, p.Key)
	if err != nil {
		return Signed{}, nil, false, err
	}
	updated, attached, err := attachReceipt(request.Method, raw, result, proposed)
	if err != nil {
		return Signed{}, nil, false, err
	}
	return proposed, updated, attached, nil
}

func attachReceipt(
	method string,
	raw []byte,
	result map[string]any,
	signed Signed,
) ([]byte, bool, error) {
	carrier, err := receiptCarrier(method, result)
	if err != nil {
		return nil, false, err
	}
	metadata, ok := carrier["metadata"].(map[string]any)
	if !ok {
		if _, exists := carrier["metadata"]; exists {
			return nil, false, fmt.Errorf("A2A response metadata is not an object")
		}
		metadata = make(map[string]any)
		carrier["metadata"] = metadata
	}
	signedJSON, err := Marshal(signed)
	if err != nil {
		return nil, false, err
	}
	signedValue, err := decodeObject(signedJSON)
	if err != nil {
		return nil, false, fmt.Errorf("decode signed receipt metadata: %w", err)
	}
	metadata[ExtensionURI] = signedValue

	outer, err := decodeObject(raw)
	if err != nil {
		return nil, false, fmt.Errorf("decode A2A response envelope: %w", err)
	}
	outer["result"] = result
	updated, err := json.Marshal(outer)
	if err != nil {
		return nil, false, fmt.Errorf("encode receipt-bearing A2A response: %w", err)
	}
	return updated, true, nil
}

func receiptCarrier(method string, result map[string]any) (map[string]any, error) {
	switch method {
	case "SendMessage":
		task, ok := result["task"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("SendMessage receipt requires a Task result")
		}
		return task, nil
	case "GetTask":
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported A2A receipt method %q", method)
	}
}

func (p *Processor) validate() error {
	if !azpRE.MatchString(p.AZP) {
		return fmt.Errorf("receipt azp is invalid")
	}
	if p.Key == nil || p.KeyID == "" || p.Archive == nil || p.Pending == nil || p.Now == nil {
		return fmt.Errorf("usage receipt processor is incomplete")
	}
	return nil
}

func decodeObject(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := expectEOF(decoder); err != nil {
		return nil, err
	}
	return value, nil
}

func validateA2AResult(method string, raw json.RawMessage) (responseEvidence, error) {
	switch method {
	case "SendMessage":
		var response a2a.StreamResponse
		if err := json.Unmarshal(raw, &response); err != nil {
			return responseEvidence{}, fmt.Errorf("decode SendMessage result through a2a-go: %w", err)
		}
		switch event := response.Event.(type) {
		case *a2a.Message:
			if err := validateMessage(event); err != nil {
				return responseEvidence{}, fmt.Errorf("validate SendMessage Message result: %w", err)
			}
			if event.Role != a2a.MessageRoleAgent {
				return responseEvidence{}, fmt.Errorf("SendMessage Message result role must be ROLE_AGENT")
			}
			return responseEvidence{}, fmt.Errorf(
				"SendMessage Message result cannot prove terminal task state",
			)
		case *a2a.Task:
			if err := validateTask(event); err != nil {
				return responseEvidence{}, fmt.Errorf("validate SendMessage Task result: %w", err)
			}
			return taskResponseEvidence(event), nil
		default:
			return responseEvidence{}, fmt.Errorf("SendMessage result is not a Message or Task")
		}
	case "GetTask":
		var task a2a.Task
		if err := json.Unmarshal(raw, &task); err != nil {
			return responseEvidence{}, fmt.Errorf("decode GetTask result through a2a-go: %w", err)
		}
		if err := validateTask(&task); err != nil {
			return responseEvidence{}, fmt.Errorf("validate GetTask Task result: %w", err)
		}
		return taskResponseEvidence(&task), nil
	default:
		return responseEvidence{}, fmt.Errorf("unsupported A2A receipt method %q", method)
	}
}

func validateMessage(message *a2a.Message) error {
	if message == nil {
		return fmt.Errorf("message is null")
	}
	if !validIdentifier(message.ID) {
		return fmt.Errorf("messageId is invalid")
	}
	if message.Role != a2a.MessageRoleAgent && message.Role != a2a.MessageRoleUser {
		return fmt.Errorf("role is invalid")
	}
	if len(message.Parts) == 0 {
		return fmt.Errorf("parts are missing")
	}
	for _, part := range message.Parts {
		if part == nil || part.Content == nil {
			return fmt.Errorf("part content is invalid")
		}
	}
	if message.ContextID != "" && !validIdentifier(message.ContextID) {
		return fmt.Errorf("contextId is invalid")
	}
	if message.TaskID != "" && !validIdentifier(string(message.TaskID)) {
		return fmt.Errorf("taskId is invalid")
	}
	for _, taskID := range message.ReferenceTasks {
		if !validIdentifier(string(taskID)) {
			return fmt.Errorf("referenceTaskIds contains an invalid task ID")
		}
	}
	return nil
}

func validateTask(task *a2a.Task) error {
	if task == nil {
		return fmt.Errorf("task is null")
	}
	if !validIdentifier(string(task.ID)) {
		return fmt.Errorf("id is invalid")
	}
	if !validIdentifier(task.ContextID) {
		return fmt.Errorf("contextId is invalid")
	}
	if !validTaskStates[task.Status.State] {
		return fmt.Errorf("status.state is invalid")
	}
	if task.Status.Message != nil {
		if err := validateMessage(task.Status.Message); err != nil {
			return fmt.Errorf("status.message is invalid: %w", err)
		}
	}
	for _, message := range task.History {
		if err := validateMessage(message); err != nil {
			return fmt.Errorf("history message is invalid: %w", err)
		}
	}
	for _, artifact := range task.Artifacts {
		if artifact == nil || !validIdentifier(string(artifact.ID)) || len(artifact.Parts) == 0 {
			return fmt.Errorf("artifact is invalid")
		}
		for _, part := range artifact.Parts {
			if part == nil || part.Content == nil {
				return fmt.Errorf("artifact part content is invalid")
			}
		}
	}
	return nil
}

func taskResponseEvidence(task *a2a.Task) responseEvidence {
	return responseEvidence{
		TaskID:    string(task.ID),
		ContextID: task.ContextID,
		Outcome:   string(task.Status.State),
		Terminal:  task.Status.State.Terminal(),
	}
}

func expectEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}
