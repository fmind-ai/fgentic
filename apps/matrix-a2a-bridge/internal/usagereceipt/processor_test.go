package usagereceipt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProcessorInjectsAndArchivesTerminalReceipt(t *testing.T) {
	processor, archivePath := testProcessor(t)
	request, err := ParseRequest([]byte(`{
  "jsonrpc":"2.0","id":"request-1","method":"SendMessage","params":{"message":{
    "messageId":"message-1","role":"ROLE_USER","parts":[{"text":"untrusted prompt"}],
    "metadata":{"https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":9007199254740993}}
  }}}
`))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	response := []byte(`{"jsonrpc":"2.0","id":9007199254740993,"result":{"kind":"message","messageId":"reply-1","taskId":"task-1","contextId":"context-1","role":"ROLE_AGENT","parts":[{"text":"reply"}]}}`)
	updated, attached, err := processor.TransformResponse(request, response)
	if err != nil {
		t.Fatalf("TransformResponse: %v", err)
	}
	if !attached {
		t.Fatal("TransformResponse did not attach a terminal receipt")
	}
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(updated, &envelope); err != nil {
		t.Fatalf("decode updated response envelope: %v", err)
	}
	if string(envelope.ID) != "9007199254740993" {
		t.Fatalf("numeric JSON-RPC id changed during receipt injection: %s", envelope.ID)
	}
	signed := receiptFromResponse(t, updated)
	if err := Verify(signed, &processor.Key.PublicKey, processor.KeyID); err != nil {
		t.Fatalf("Verify response receipt: %v", err)
	}
	if signed.Receipt.TokensReserved != 9007199254740993 || signed.Receipt.TokensConsumed != nil ||
		signed.Receipt.AZP != "org-b-a2a" || signed.Receipt.TaskID != "task-1" ||
		signed.Receipt.RequestHash != request.RequestHash {
		t.Fatalf("response receipt = %+v", signed.Receipt)
	}
	archive, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("ReadFile archive: %v", err)
	}
	if strings.Count(string(archive), "\n") != 1 || strings.Contains(string(archive), "untrusted prompt") {
		t.Fatalf("archive is not one content-free JSONL record: %q", archive)
	}
	archived, err := Parse([]byte(strings.TrimSpace(string(archive))))
	if err != nil {
		t.Fatalf("Parse archive: %v", err)
	}
	if !reflect.DeepEqual(archived.Receipt, signed.Receipt) {
		t.Fatalf("archived receipt differs from delivered receipt: %+v != %+v", archived, signed)
	}
}

func TestProcessorCorrelatesWorkingTaskToGetTask(t *testing.T) {
	processor, archivePath := testProcessor(t)
	request, err := ParseRequest([]byte(`{"jsonrpc":"2.0","id":"request-1","method":"SendMessage","params":{"message":{"metadata":{"https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":1000}}}}}`))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	working := []byte(`{"jsonrpc":"2.0","id":"request-1","result":{"kind":"task","id":"task-2","contextId":"context-2","status":{"state":"TASK_STATE_WORKING"}}}`)
	if updated, attached, err := processor.TransformResponse(request, working); err != nil || attached || string(updated) != string(working) {
		t.Fatalf("working response = attached %v, err %v, body %s", attached, err, updated)
	}
	getTask, err := ParseRequest([]byte(`{"jsonrpc":"2.0","id":"request-2","method":"GetTask","params":{"id":"task-2"}}`))
	if err != nil {
		t.Fatalf("ParseRequest GetTask: %v", err)
	}
	completed := []byte(`{"jsonrpc":"2.0","id":"request-2","result":{"kind":"task","id":"task-2","contextId":"context-2","status":{"state":"TASK_STATE_COMPLETED"}}}`)
	updated, attached, err := processor.TransformResponse(getTask, completed)
	if err != nil || !attached {
		t.Fatalf("completed response = attached %v, err %v", attached, err)
	}
	signed := receiptFromResponse(t, updated)
	if signed.Receipt.TokensReserved != 1000 || signed.Receipt.Outcome != "TASK_STATE_COMPLETED" {
		t.Fatalf("completed receipt = %+v", signed.Receipt)
	}
	if _, found, err := processor.Pending.Load("task-2"); err != nil || found {
		t.Fatalf("pending evidence survived completion: found %v, err %v", found, err)
	}
	if archive, err := os.ReadFile(archivePath); err != nil || strings.Count(string(archive), "\n") != 1 {
		t.Fatalf("archive after GetTask = %q, err %v", archive, err)
	}
	retried, attached, err := processor.TransformResponse(getTask, completed)
	if err != nil || !attached {
		t.Fatalf("retried completed response = attached %v, err %v", attached, err)
	}
	if retriedReceipt := receiptFromResponse(t, retried); !reflect.DeepEqual(retriedReceipt, signed) {
		t.Fatalf("retried receipt differs from first delivery: %+v != %+v", retriedReceipt, signed)
	}
	if archive, err := os.ReadFile(archivePath); err != nil || strings.Count(string(archive), "\n") != 1 {
		t.Fatalf("archive after retried GetTask = %q, err %v", archive, err)
	}
}

func TestArchiveRejectsConflictingAssertionForTask(t *testing.T) {
	processor, _ := testProcessor(t)
	first, err := New(
		processor.AZP, "task-3", "context-3", "sha256:"+strings.Repeat("a", 64),
		10, processor.Now(), "TASK_STATE_COMPLETED", processor.KeyID,
	)
	if err != nil {
		t.Fatalf("New first receipt: %v", err)
	}
	firstSigned, err := Sign(first, processor.Key)
	if err != nil {
		t.Fatalf("Sign first receipt: %v", err)
	}
	if _, err := processor.Archive.AppendUnique(firstSigned); err != nil {
		t.Fatalf("AppendUnique first receipt: %v", err)
	}
	conflict := first
	conflict.ContextID = "different-context"
	conflictingSigned, err := Sign(conflict, processor.Key)
	if err != nil {
		t.Fatalf("Sign conflicting receipt: %v", err)
	}
	if _, err := processor.Archive.AppendUnique(conflictingSigned); err == nil {
		t.Fatal("AppendUnique accepted a conflicting task assertion")
	}
}

func TestPendingStoreRejectsConflictingTaskEvidence(t *testing.T) {
	store := &PendingStore{Dir: t.TempDir()}
	first := pendingEvidence{RequestHash: "sha256:" + strings.Repeat("a", 64), TokensReserved: 10}
	if err := store.Save("task with spaces", first); err != nil {
		t.Fatalf("Save first evidence: %v", err)
	}
	if err := store.Save("task with spaces", first); err != nil {
		t.Fatalf("Save identical evidence: %v", err)
	}
	second := pendingEvidence{RequestHash: "sha256:" + strings.Repeat("b", 64), TokensReserved: 10}
	if err := store.Save("task with spaces", second); err == nil {
		t.Fatal("Save conflicting evidence succeeded")
	}
}

func TestProcessorDoesNotCreateReceiptForUnauthorizedOrErrorPath(t *testing.T) {
	processor, archivePath := testProcessor(t)
	request, err := ParseRequest([]byte(`{"jsonrpc":"2.0","id":"request-1","method":"ListTasks","params":{}}`))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	errorResponse := []byte(`{"jsonrpc":"2.0","id":"request-1","error":{"code":-32601,"message":"denied"}}`)
	updated, attached, err := processor.TransformResponse(request, errorResponse)
	if err != nil || attached || string(updated) != string(errorResponse) {
		t.Fatalf("denied response = attached %v, err %v, body %s", attached, err, updated)
	}
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("unauthorized path created an archive: %v", err)
	}
}

func testProcessor(t *testing.T) (*Processor, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	directory := t.TempDir()
	archivePath := filepath.Join(directory, "receipts.jsonl")
	return &Processor{
		AZP: "org-b-a2a", KeyID: "fgentic-org-a-receipts-v1", Key: key,
		Archive: &Archive{Path: archivePath}, Pending: &PendingStore{Dir: filepath.Join(directory, "pending")},
		Now: func() time.Time { return time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC) },
	}, archivePath
}

func receiptFromResponse(t *testing.T, raw []byte) Signed {
	t.Helper()
	var envelope struct {
		Result struct {
			Metadata map[string]json.RawMessage `json:"metadata"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	receiptRaw := envelope.Result.Metadata[ExtensionURI]
	if len(receiptRaw) == 0 {
		t.Fatalf("response has no %s metadata: %s", ExtensionURI, raw)
	}
	signed, err := Parse(receiptRaw)
	if err != nil {
		t.Fatalf("Parse response receipt: %v", err)
	}
	return signed
}
