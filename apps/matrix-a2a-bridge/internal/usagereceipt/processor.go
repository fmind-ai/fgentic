package usagereceipt

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gowebpki/jcs"
)

// ExtensionURI identifies the versioned receipt in A2A activation and metadata fields.
const ExtensionURI = "https://fgentic.fmind.ai/a2a/extensions/usage-receipt/v1"

var terminalTaskStates = map[string]bool{
	"TASK_STATE_COMPLETED": true,
	"TASK_STATE_FAILED":    true,
	"TASK_STATE_CANCELED":  true,
	"TASK_STATE_REJECTED":  true,
}

type requestEvidence struct {
	Method         string `json:"method"`
	TaskID         string `json:"taskId,omitempty"`
	RequestHash    string `json:"requestHash,omitempty"`
	TokensReserved uint64 `json:"tokensReserved,omitempty"`
}

type pendingEvidence struct {
	RequestHash    string `json:"requestHash"`
	TokensReserved uint64 `json:"tokensReserved"`
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
}

// ParseRequest records only the content-free evidence needed for a later receipt.
func ParseRequest(raw []byte) (requestEvidence, error) {
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return requestEvidence{}, fmt.Errorf("canonicalize A2A request: %w", err)
	}
	var request struct {
		Method string `json:"method"`
		Params struct {
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
		if err := budgetDecoder.Decode(&budget); err != nil || budget.MaxTokens == 0 {
			return requestEvidence{}, fmt.Errorf("SendMessage token-budget metadata is invalid")
		}
		digest := sha256.Sum256(canonical)
		return requestEvidence{
			Method: request.Method, RequestHash: "sha256:" + hex.EncodeToString(digest[:]),
			TokensReserved: budget.MaxTokens,
		}, nil
	case "GetTask":
		if !validIdentifier(request.Params.ID) {
			return requestEvidence{}, fmt.Errorf("GetTask id is invalid")
		}
		return requestEvidence{Method: request.Method, TaskID: request.Params.ID}, nil
	default:
		return requestEvidence{}, nil
	}
}

// TransformResponse signs and injects a receipt when raw contains a terminal result. Nonterminal,
// JSON-RPC error, and unrelated-method responses pass through byte-for-byte.
func (p *Processor) TransformResponse(request requestEvidence, raw []byte) ([]byte, bool, error) {
	if request.Method != "SendMessage" && request.Method != "GetTask" {
		return raw, false, nil
	}
	if err := p.validate(); err != nil {
		return nil, false, err
	}
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Result) == 0 || string(envelope.Error) != "" && string(envelope.Error) != "null" {
		return raw, false, nil
	}
	result, err := decodeObject(envelope.Result)
	if err != nil {
		return raw, false, nil
	}
	taskID, contextID, outcome, terminal := resultEvidence(result)
	if taskID == "" || contextID == "" {
		return raw, false, nil
	}

	if request.Method == "GetTask" {
		if request.TaskID != taskID {
			return nil, false, fmt.Errorf("GetTask response task ID does not match request")
		}
	}
	evidence := pendingEvidence{RequestHash: request.RequestHash, TokensReserved: request.TokensReserved}
	if !terminal {
		if request.Method == "SendMessage" {
			if err := p.Pending.Save(taskID, evidence); err != nil {
				return nil, false, err
			}
		}
		return raw, false, nil
	}
	archived, found, err := p.Archive.Find(taskID)
	if err != nil {
		return nil, false, err
	}
	if found {
		if err := p.validateArchived(archived, request, taskID, contextID, outcome); err != nil {
			return nil, false, err
		}
		if request.Method == "GetTask" {
			if err := p.deletePending(taskID, archived.Receipt); err != nil {
				return nil, false, err
			}
		}
		return attachReceipt(raw, result, archived)
	}
	if request.Method == "GetTask" {
		evidence, found, err = p.Pending.Load(taskID)
		if err != nil {
			return nil, false, err
		}
		if !found {
			return raw, false, nil
		}
	}
	if evidence.RequestHash == "" || evidence.TokensReserved == 0 {
		return nil, false, fmt.Errorf("terminal A2A response has no reservation evidence")
	}
	receipt, err := New(
		p.AZP, taskID, contextID, evidence.RequestHash, evidence.TokensReserved,
		p.Now(), outcome, p.KeyID,
	)
	if err != nil {
		return nil, false, err
	}
	proposed, err := Sign(receipt, p.Key)
	if err != nil {
		return nil, false, err
	}
	signed, err := p.Archive.AppendUnique(proposed)
	if err != nil {
		return nil, false, err
	}
	if err := p.validateArchived(signed, request, taskID, contextID, outcome); err != nil {
		return nil, false, err
	}
	if request.Method == "GetTask" {
		if err := p.deletePending(taskID, signed.Receipt); err != nil {
			return nil, false, err
		}
	}
	return attachReceipt(raw, result, signed)
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

func (p *Processor) deletePending(taskID string, receipt Receipt) error {
	evidence, found, err := p.Pending.Load(taskID)
	if err != nil {
		return err
	}
	if found && (evidence.RequestHash != receipt.RequestHash ||
		evidence.TokensReserved != receipt.TokensReserved) {
		return fmt.Errorf("pending usage receipt evidence conflicts with archive")
	}
	return p.Pending.Delete(taskID)
}

func attachReceipt(raw []byte, result map[string]any, signed Signed) ([]byte, bool, error) {
	metadata, ok := result["metadata"].(map[string]any)
	if !ok {
		metadata = make(map[string]any)
		result["metadata"] = metadata
	}
	signedJSON, err := Marshal(signed)
	if err != nil {
		return nil, false, err
	}
	var signedValue any
	if err := json.Unmarshal(signedJSON, &signedValue); err != nil {
		return nil, false, fmt.Errorf("decode signed receipt metadata: %w", err)
	}
	metadata[ExtensionURI] = signedValue

	var outer map[string]any
	if err := json.Unmarshal(raw, &outer); err != nil {
		return nil, false, fmt.Errorf("decode A2A response envelope: %w", err)
	}
	outer["result"] = result
	updated, err := json.Marshal(outer)
	if err != nil {
		return nil, false, fmt.Errorf("encode receipt-bearing A2A response: %w", err)
	}
	return updated, true, nil
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

func resultEvidence(result map[string]any) (taskID, contextID, outcome string, terminal bool) {
	kind, _ := result["kind"].(string)
	switch strings.ToLower(kind) {
	case "message":
		taskID, _ = result["taskId"].(string)
		contextID, _ = result["contextId"].(string)
		return taskID, contextID, "TASK_STATE_COMPLETED", taskID != "" && contextID != ""
	case "task":
		taskID, _ = result["id"].(string)
		contextID, _ = result["contextId"].(string)
		status, _ := result["status"].(map[string]any)
		outcome, _ = status["state"].(string)
		return taskID, contextID, outcome, terminalTaskStates[outcome]
	default:
		return "", "", "", false
	}
}

func expectEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}
