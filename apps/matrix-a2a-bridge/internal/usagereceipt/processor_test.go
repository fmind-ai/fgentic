package usagereceipt

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func TestProcessorInjectsAndArchivesTerminalReceipt(t *testing.T) {
	processor, archivePath := testProcessor(t)
	request, err := ParseRequest([]byte(`{
  "jsonrpc":"2.0","id":9007199254740992,"method":"SendMessage","params":{"message":{
    "messageId":"message-1","role":"ROLE_USER","parts":[{"text":"untrusted prompt"}],
    "metadata":{"https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":3000}}
  }}}
`))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	response := []byte(`{"jsonrpc":"2.0","id":9007199254740992,"result":{"task":{
	  "id":"task-1","contextId":"context-1",
	  "status":{"state":"TASK_STATE_COMPLETED","message":{
	    "messageId":"reply-1","role":"ROLE_AGENT","parts":[{"text":"reply"}]
	  }}
	}}}`)
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
	if string(envelope.ID) != "9007199254740992" {
		t.Fatalf("numeric JSON-RPC id changed during receipt injection: %s", envelope.ID)
	}
	var sdkEnvelope struct {
		Result a2a.StreamResponse `json:"result"`
	}
	if err := json.Unmarshal(updated, &sdkEnvelope); err != nil {
		t.Fatalf("decode updated response through a2a-go: %v", err)
	}
	task, ok := sdkEnvelope.Result.Event.(*a2a.Task)
	if !ok || task.Metadata[ExtensionURI] == nil {
		t.Fatalf(
			"a2a-go task did not retain usage receipt metadata: %#v",
			sdkEnvelope.Result.Event,
		)
	}
	signed := receiptFromResponse(t, updated)
	if err := Verify(signed, &processor.Key.PublicKey, processor.KeyID); err != nil {
		t.Fatalf("Verify response receipt: %v", err)
	}
	if signed.Receipt.TokensReserved != 3000 || signed.Receipt.TokensConsumed != nil ||
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

func TestProcessorConcurrentCompletionReturnsArchivedReceipt(t *testing.T) {
	processor, archivePath := testProcessor(t)
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	completionTimes := make(chan time.Time, 2)
	completionTimes <- time.Date(2026, 7, 16, 9, 0, 1, 0, time.UTC)
	completionTimes <- time.Date(2026, 7, 16, 9, 0, 2, 0, time.UTC)
	processor.Now = func() time.Time {
		ready <- struct{}{}
		<-release
		return <-completionTimes
	}
	request, err := ParseRequest([]byte(`{
	  "jsonrpc":"2.0","id":"request-concurrent","method":"SendMessage","params":{"message":{
	    "messageId":"message-concurrent","role":"ROLE_USER","parts":[{"text":"untrusted prompt"}],
	    "metadata":{"https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":3000}}
	  }}
	}`))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	response := []byte(`{
	  "jsonrpc":"2.0","id":"request-concurrent","result":{"task":{
	    "id":"task-concurrent","contextId":"context-concurrent",
	    "status":{"state":"TASK_STATE_COMPLETED","message":{
	      "messageId":"reply-concurrent","role":"ROLE_AGENT","parts":[{"text":"reply"}]
	    }}
	  }}
	}`)
	type transformResult struct {
		updated  []byte
		attached bool
		err      error
	}
	results := make(chan transformResult, 2)
	for range 2 {
		go func() {
			updated, attached, err := processor.TransformResponse(request, response)
			results <- transformResult{updated: updated, attached: attached, err: err}
		}()
	}
	arrived := true
	for range 2 {
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			arrived = false
		}
	}
	close(release)
	if !arrived {
		t.Fatal("concurrent completions did not both reach the pre-archive barrier")
	}
	var returned [2]Signed
	for index := range returned {
		select {
		case result := <-results:
			if result.err != nil || !result.attached {
				t.Fatalf(
					"concurrent completion = attached %v, err %v, body %s",
					result.attached,
					result.err,
					result.updated,
				)
			}
			returned[index] = receiptFromResponse(t, result.updated)
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent completion did not return")
		}
	}
	if !reflect.DeepEqual(returned[0], returned[1]) {
		t.Fatalf("concurrent completions returned different receipts: %+v != %+v", returned[0], returned[1])
	}
	archive, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("ReadFile archive: %v", err)
	}
	if strings.Count(string(archive), "\n") != 1 {
		t.Fatalf("concurrent completions appended multiple receipts: %q", archive)
	}
	archived, err := Parse([]byte(strings.TrimSpace(string(archive))))
	if err != nil {
		t.Fatalf("Parse archived receipt: %v", err)
	}
	if !reflect.DeepEqual(archived, returned[0]) {
		t.Fatalf("returned receipt differs from archive: %+v != %+v", returned[0], archived)
	}
}

func TestProcessorCorrelatesWorkingTaskToGetTask(t *testing.T) {
	processor, archivePath := testProcessor(t)
	request, err := ParseRequest([]byte(`{"jsonrpc":"2.0","id":"request-1","method":"SendMessage","params":{"message":{"metadata":{"https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":1000}}}}}`))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	working := []byte(`{"jsonrpc":"2.0","id":"request-1","result":{"task":{"id":"task-2","contextId":"context-2","status":{"state":"TASK_STATE_WORKING"}}}}`)
	if updated, attached, err := processor.TransformResponse(request, working); err != nil || attached || string(updated) != string(working) {
		t.Fatalf("working response = attached %v, err %v, body %s", attached, err, updated)
	}
	getTask, err := ParseRequest([]byte(`{"jsonrpc":"2.0","id":"request-2","method":"GetTask","params":{"id":"task-2"}}`))
	if err != nil {
		t.Fatalf("ParseRequest GetTask: %v", err)
	}
	completed := []byte(`{"jsonrpc":"2.0","id":"request-2","result":{"id":"task-2","contextId":"context-2","status":{"state":"TASK_STATE_COMPLETED"}}}`)
	updated, attached, err := processor.TransformResponse(getTask, completed)
	if err != nil || !attached {
		t.Fatalf("completed response = attached %v, err %v", attached, err)
	}
	var taskEnvelope struct {
		Result a2a.Task `json:"result"`
	}
	if err := json.Unmarshal(updated, &taskEnvelope); err != nil {
		t.Fatalf("decode updated task response through a2a-go: %v", err)
	}
	if taskEnvelope.Result.Metadata[ExtensionURI] == nil {
		t.Fatalf("a2a-go task did not retain usage receipt metadata: %#v", taskEnvelope.Result)
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

func TestRequestHashCanonicalizationRejectsUnsafeIntegers(t *testing.T) {
	first, err := RequestHash([]byte(`{"jsonrpc":"2.0","id":"request-1","value":0.1}`))
	if err != nil {
		t.Fatalf("RequestHash first: %v", err)
	}
	second, err := RequestHash([]byte("{\n  \"value\": 0.1, \"id\": \"request-1\", \"jsonrpc\": \"2.0\"\n}"))
	if err != nil {
		t.Fatalf("RequestHash second: %v", err)
	}
	if first != second {
		t.Fatalf("canonical request hashes differ: %s != %s", first, second)
	}
	if _, err := RequestHash([]byte(`{"jsonrpc":"2.0","id":9007199254740992}`)); err != nil {
		t.Fatalf("RequestHash rejected an exactly representable integer: %v", err)
	}
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":9007199254740993}`,
		`{"jsonrpc":"2.0","id":"request-1","metadata":{"unsafe":-9007199254740993}}`,
		`{"jsonrpc":"2.0","id":"request-1","metadata":{"unsafe":9007199254740992.5}}`,
	} {
		if _, err := RequestHash([]byte(raw)); err == nil {
			t.Fatalf("RequestHash accepted unsafe integer: %s", raw)
		}
	}
}

func TestCanonicalJSONRPCIDPreservesTypeAndValue(t *testing.T) {
	number, err := canonicalJSONRPCID(json.RawMessage(`1.0`))
	if err != nil {
		t.Fatalf("canonicalJSONRPCID number: %v", err)
	}
	equivalent, err := canonicalJSONRPCID(json.RawMessage(`1`))
	if err != nil {
		t.Fatalf("canonicalJSONRPCID equivalent number: %v", err)
	}
	if number != equivalent {
		t.Fatalf("equivalent numeric ids differ: %s != %s", number, equivalent)
	}
	stringID, err := canonicalJSONRPCID(json.RawMessage(`"1"`))
	if err != nil {
		t.Fatalf("canonicalJSONRPCID string: %v", err)
	}
	if stringID == number {
		t.Fatalf("string and numeric ids collapsed to %s", stringID)
	}
	if nullID, err := canonicalJSONRPCID(json.RawMessage(`null`)); err != nil || nullID != "null" {
		t.Fatalf("canonicalJSONRPCID null = %q, err %v", nullID, err)
	}
	for _, raw := range []string{"", "true", "[]", "{}", "9007199254740993"} {
		if _, err := canonicalJSONRPCID(json.RawMessage(raw)); err == nil {
			t.Fatalf("canonicalJSONRPCID accepted invalid id: %s", raw)
		}
	}
}

func TestParseRequestRejectsInvalidJSONRPCEnvelope(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "missing version", raw: `{"id":"request-1","method":"GetTask","params":{"id":"task-1"}}`},
		{name: "wrong version", raw: `{"jsonrpc":"1.0","id":"request-1","method":"GetTask","params":{"id":"task-1"}}`},
		{name: "missing id", raw: `{"jsonrpc":"2.0","method":"GetTask","params":{"id":"task-1"}}`},
		{name: "boolean id", raw: `{"jsonrpc":"2.0","id":true,"method":"GetTask","params":{"id":"task-1"}}`},
		{name: "object id", raw: `{"jsonrpc":"2.0","id":{},"method":"GetTask","params":{"id":"task-1"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseRequest([]byte(test.raw)); err == nil {
				t.Fatalf("ParseRequest accepted invalid JSON-RPC envelope: %s", test.raw)
			}
		})
	}
}

func TestProcessorDoesNotPersistInvalidJSONRPCResponseEvidence(t *testing.T) {
	envelopes := []struct {
		name      string
		requestID string
		response  string
	}{
		{name: "missing version", requestID: `"1"`, response: `{"id":"1","result":$RESULT}`},
		{
			name:      "wrong version",
			requestID: `"1"`,
			response:  `{"jsonrpc":"1.0","id":"1","result":$RESULT}`,
		},
		{
			name:      "missing id",
			requestID: `"1"`,
			response:  `{"jsonrpc":"2.0","result":$RESULT}`,
		},
		{
			name:      "mismatched id",
			requestID: `"1"`,
			response:  `{"jsonrpc":"2.0","id":"2","result":$RESULT}`,
		},
		{
			name:      "mismatched type",
			requestID: `"1"`,
			response:  `{"jsonrpc":"2.0","id":1,"result":$RESULT}`,
		},
		{
			name:      "unsafe numeric id",
			requestID: `9007199254740992`,
			response:  `{"jsonrpc":"2.0","id":9007199254740993,"result":$RESULT}`,
		},
	}
	results := []struct {
		name string
		body string
	}{
		{name: "working", body: `{"task":{"id":"task-invalid","contextId":"context-invalid","status":{"state":"TASK_STATE_WORKING"}}}`},
		{name: "terminal", body: `{"task":{"id":"task-invalid","contextId":"context-invalid","status":{"state":"TASK_STATE_COMPLETED"}}}`},
	}
	for _, envelope := range envelopes {
		for _, result := range results {
			t.Run(envelope.name+"/"+result.name, func(t *testing.T) {
				processor, archivePath := testProcessor(t)
				requestRaw := strings.Replace(`{
				  "jsonrpc":"2.0","id":$ID,"method":"SendMessage","params":{"message":{"metadata":{
				    "https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":1000}
				  }}}
				}`, "$ID", envelope.requestID, 1)
				request, err := ParseRequest([]byte(requestRaw))
				if err != nil {
					t.Fatalf("ParseRequest: %v", err)
				}
				response := []byte(strings.Replace(envelope.response, "$RESULT", result.body, 1))
				updated, attached, err := processor.TransformResponse(request, response)
				if err == nil || attached || updated != nil {
					t.Fatalf(
						"invalid response = attached %v, err %v, body %s",
						attached,
						err,
						updated,
					)
				}
				if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
					t.Fatalf("invalid response created an archive: %v", err)
				}
				if _, found, err := processor.Pending.Load("task-invalid"); err != nil || found {
					t.Fatalf("invalid response persisted pending evidence: found %v, err %v", found, err)
				}
			})
		}
	}
}

func TestProcessorAcceptsEquivalentJSONRPCResponseIDs(t *testing.T) {
	for _, ids := range []struct {
		name     string
		request  string
		response string
	}{
		{name: "null", request: "null", response: "null"},
		{name: "equivalent numbers", request: "1.0", response: "1"},
	} {
		t.Run(ids.name, func(t *testing.T) {
			processor, _ := testProcessor(t)
			requestRaw := strings.Replace(`{
			  "jsonrpc":"2.0","id":$ID,"method":"SendMessage","params":{"message":{"metadata":{
			    "https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":1000}
			  }}}
			}`, "$ID", ids.request, 1)
			request, err := ParseRequest([]byte(requestRaw))
			if err != nil {
				t.Fatalf("ParseRequest: %v", err)
			}
			response := []byte(strings.Replace(
				`{"jsonrpc":"2.0","id":$ID,"result":{
				  "task":{
				    "id":"task-equivalent","contextId":"context-equivalent",
				    "status":{"state":"TASK_STATE_COMPLETED"}
				  }
				}}`,
				"$ID",
				ids.response,
				1,
			))
			if _, attached, err := processor.TransformResponse(request, response); err != nil || !attached {
				t.Fatalf("equivalent response = attached %v, err %v", attached, err)
			}
		})
	}
}

func TestProcessorRetainsPendingEvidenceForMismatchedGetTaskResponse(t *testing.T) {
	processor, archivePath := testProcessor(t)
	evidence := pendingEvidence{
		RequestHash: "sha256:" + strings.Repeat("a", 64), TokensReserved: 1000,
	}
	if err := processor.Pending.Save("task-pending", evidence); err != nil {
		t.Fatalf("Save pending evidence: %v", err)
	}
	request, err := ParseRequest([]byte(
		`{"jsonrpc":"2.0","id":"request-1","method":"GetTask","params":{"id":"task-pending"}}`,
	))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	response := []byte(`{
	  "jsonrpc":"2.0","id":"different-request","result":{
	    "id":"task-pending","contextId":"context-pending",
	    "status":{"state":"TASK_STATE_COMPLETED"}
	  }
	}`)
	updated, attached, err := processor.TransformResponse(request, response)
	if err == nil || attached || updated != nil {
		t.Fatalf("mismatched GetTask response = attached %v, err %v, body %s", attached, err, updated)
	}
	retained, found, err := processor.Pending.Load("task-pending")
	if err != nil || !found || !reflect.DeepEqual(retained, evidence) {
		t.Fatalf("pending evidence = %+v, found %v, err %v", retained, found, err)
	}
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("mismatched GetTask response created an archive: %v", err)
	}
}

func TestProcessorRejectsNonObjectResultMetadata(t *testing.T) {
	processor, archivePath := testProcessor(t)
	request, err := ParseRequest([]byte(`{
  "jsonrpc":"2.0",
  "id":"request-1",
  "method":"SendMessage",
  "params":{"message":{"metadata":{
    "https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":1000}
  }}}
}`))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	response := []byte(`{
  "jsonrpc":"2.0",
  "id":"request-1",
  "result":{"task":{
    "id":"task-1",
    "contextId":"context-1",
    "status":{"state":"TASK_STATE_COMPLETED"},
    "metadata":"invalid"
  }}
}`)
	if _, _, err := processor.TransformResponse(request, response); err == nil {
		t.Fatal("TransformResponse replaced non-object A2A metadata")
	}
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("malformed metadata created an archive: %v", err)
	}
	if _, found, err := processor.Pending.Load("task-1"); err != nil || found {
		t.Fatalf("malformed metadata persisted pending evidence: found %v, err %v", found, err)
	}
}

func TestProcessorRejectsDirectMessageWithoutTaskCompletionState(t *testing.T) {
	processor, archivePath := testProcessor(t)
	request, err := ParseRequest([]byte(`{
  "jsonrpc":"2.0",
  "id":"request-1",
  "method":"SendMessage",
  "params":{"message":{"metadata":{
    "https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":1000}
  }}}
}`))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	response := []byte(`{
  "jsonrpc":"2.0",
  "id":"request-1",
  "result":{"message":{
    "messageId":"reply-1",
    "contextId":"context-1",
    "role":"ROLE_AGENT",
    "parts":[{"text":"reply"}]
  }}
}`)
	if _, _, err := processor.TransformResponse(request, response); err == nil {
		t.Fatal("TransformResponse treated a direct Message as terminal task evidence")
	}
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("direct Message created an archive: %v", err)
	}
	if _, found, err := processor.Pending.Load("task-1"); err != nil || found {
		t.Fatalf("direct Message persisted pending evidence: found %v, err %v", found, err)
	}
}

func TestProcessorDoesNotPersistInvalidSendMessageResult(t *testing.T) {
	tests := []struct {
		name     string
		response string
	}{
		{
			name: "direct working task",
			response: `{
			  "jsonrpc":"2.0","id":"request-1","result":{
			    "id":"task-invalid","contextId":"context-invalid",
			    "status":{"state":"TASK_STATE_WORKING"}
			  }
			}`,
		},
		{
			name: "malformed wrapped working task",
			response: `{
			  "jsonrpc":"2.0","id":"request-1","result":{"task":{
			    "id":"task-invalid","contextId":"context-invalid",
			    "status":{"state":"TASK_STATE_WORKING"},"metadata":"invalid"
			  }}
			}`,
		},
		{
			name: "ambiguous terminal wrappers",
			response: `{
			  "jsonrpc":"2.0","id":"request-1","result":{
			    "message":{
			      "messageId":"reply-invalid","taskId":"task-invalid",
			      "contextId":"context-invalid","role":"ROLE_AGENT","parts":[]
			    },
			    "task":{
			      "id":"task-invalid","contextId":"context-invalid",
			      "status":{"state":"TASK_STATE_COMPLETED"}
			    }
			  }
			}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			processor, archivePath := testProcessor(t)
			request, err := ParseRequest([]byte(`{
			  "jsonrpc":"2.0","id":"request-1","method":"SendMessage","params":{"message":{"metadata":{
			    "https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":1000}
			  }}}
			}`))
			if err != nil {
				t.Fatalf("ParseRequest: %v", err)
			}
			updated, attached, err := processor.TransformResponse(request, []byte(test.response))
			if err == nil || attached || updated != nil {
				t.Fatalf(
					"invalid SendMessage result = attached %v, err %v, body %s",
					attached,
					err,
					updated,
				)
			}
			if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
				t.Fatalf("invalid SendMessage result created an archive: %v", err)
			}
			if _, found, err := processor.Pending.Load("task-invalid"); err != nil || found {
				t.Fatalf(
					"invalid SendMessage result persisted pending evidence: found %v, err %v",
					found,
					err,
				)
			}
		})
	}
}

func TestProcessorRejectsSemanticallyInvalidA2AResults(t *testing.T) {
	tests := []struct {
		name   string
		result string
	}{
		{
			name: "message missing id",
			result: `{"message":{
			  "taskId":"task-invalid","contextId":"context-invalid",
			  "role":"ROLE_AGENT","parts":[{"text":"reply"}]
			}}`,
		},
		{
			name: "message missing role",
			result: `{"message":{
			  "messageId":"reply-invalid","taskId":"task-invalid",
			  "contextId":"context-invalid","parts":[{"text":"reply"}]
			}}`,
		},
		{
			name: "message unknown role",
			result: `{"message":{
			  "messageId":"reply-invalid","taskId":"task-invalid",
			  "contextId":"context-invalid","role":"ROLE_OTHER","parts":[{"text":"reply"}]
			}}`,
		},
		{
			name: "message user role",
			result: `{"message":{
			  "messageId":"reply-invalid","taskId":"task-invalid",
			  "contextId":"context-invalid","role":"ROLE_USER","parts":[{"text":"reply"}]
			}}`,
		},
		{
			name: "valid direct message has no terminal task state",
			result: `{"message":{
			  "messageId":"reply-invalid",
			  "role":"ROLE_AGENT","parts":[{"text":"reply"}]
			}}`,
		},
		{
			name: "message empty parts",
			result: `{"message":{
			  "messageId":"reply-invalid","taskId":"task-invalid",
			  "contextId":"context-invalid","role":"ROLE_AGENT","parts":[]
			}}`,
		},
		{
			name: "message null part",
			result: `{"message":{
			  "messageId":"reply-invalid","taskId":"task-invalid",
			  "contextId":"context-invalid","role":"ROLE_AGENT","parts":[null]
			}}`,
		},
		{
			name: "task missing id",
			result: `{"task":{
			  "contextId":"context-invalid","status":{"state":"TASK_STATE_WORKING"}
			}}`,
		},
		{
			name: "task missing context",
			result: `{"task":{
			  "id":"task-invalid","status":{"state":"TASK_STATE_WORKING"}
			}}`,
		},
		{
			name: "task missing state",
			result: `{"task":{
			  "id":"task-invalid","contextId":"context-invalid","status":{}
			}}`,
		},
		{
			name: "task unspecified state",
			result: `{"task":{
			  "id":"task-invalid","contextId":"context-invalid",
			  "status":{"state":"TASK_STATE_UNSPECIFIED"}
			}}`,
		},
		{
			name: "task unknown state",
			result: `{"task":{
			  "id":"task-invalid","contextId":"context-invalid",
			  "status":{"state":"TASK_STATE_UNKNOWN"}
			}}`,
		},
		{
			name: "task duplicate state",
			result: `{"task":{
			  "id":"task-invalid","contextId":"context-invalid",
			  "status":{"state":"TASK_STATE_WORKING","state":"TASK_STATE_COMPLETED"}
			}}`,
		},
		{
			name: "task null history message",
			result: `{"task":{
			  "id":"task-invalid","contextId":"context-invalid",
			  "status":{"state":"TASK_STATE_WORKING"},"history":[null]
			}}`,
		},
		{
			name: "task null artifact",
			result: `{"task":{
			  "id":"task-invalid","contextId":"context-invalid",
			  "status":{"state":"TASK_STATE_WORKING"},"artifacts":[null]
			}}`,
		},
		{
			name: "task artifact missing id",
			result: `{"task":{
			  "id":"task-invalid","contextId":"context-invalid",
			  "status":{"state":"TASK_STATE_WORKING"},
			  "artifacts":[{"parts":[{"text":"artifact"}]}]
			}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			processor, archivePath := testProcessor(t)
			request, err := ParseRequest([]byte(`{
			  "jsonrpc":"2.0","id":"request-1","method":"SendMessage","params":{"message":{"metadata":{
			    "https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":1000}
			  }}}
			}`))
			if err != nil {
				t.Fatalf("ParseRequest: %v", err)
			}
			response := []byte(fmt.Sprintf(
				`{"jsonrpc":"2.0","id":"request-1","result":%s}`,
				test.result,
			))
			updated, attached, err := processor.TransformResponse(request, response)
			if err == nil || attached || updated != nil {
				t.Fatalf(
					"invalid result = attached %v, err %v, body %s",
					attached,
					err,
					updated,
				)
			}
			assertNoReceiptState(t, processor, archivePath)
		})
	}
}

func TestProcessorRejectsResultAndErrorWithoutStateMutation(t *testing.T) {
	processor, archivePath := testProcessor(t)
	request, err := ParseRequest([]byte(`{
	  "jsonrpc":"2.0","id":"request-1","method":"SendMessage","params":{"message":{"metadata":{
	    "https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":1000}
	  }}}
	}`))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	response := []byte(`{
	  "jsonrpc":"2.0","id":"request-1",
	  "result":{"task":{
	    "id":"task-invalid","contextId":"context-invalid",
	    "status":{"state":"TASK_STATE_COMPLETED"}
	  }},
	  "error":{"code":-32603,"message":"ambiguous"}
	}`)
	updated, attached, err := processor.TransformResponse(request, response)
	if err == nil || attached || updated != nil {
		t.Fatalf(
			"result plus error = attached %v, err %v, body %s",
			attached,
			err,
			updated,
		)
	}
	assertNoReceiptState(t, processor, archivePath)
}

func TestValidateA2AResultRecognizesExactTaskStates(t *testing.T) {
	for _, test := range []struct {
		state    string
		terminal bool
	}{
		{state: "TASK_STATE_SUBMITTED"},
		{state: "TASK_STATE_WORKING"},
		{state: "TASK_STATE_INPUT_REQUIRED"},
		{state: "TASK_STATE_AUTH_REQUIRED"},
		{state: "TASK_STATE_COMPLETED", terminal: true},
		{state: "TASK_STATE_FAILED", terminal: true},
		{state: "TASK_STATE_CANCELED", terminal: true},
		{state: "TASK_STATE_REJECTED", terminal: true},
	} {
		t.Run(test.state, func(t *testing.T) {
			raw := json.RawMessage(fmt.Sprintf(`{"task":{
			  "id":"task-state","contextId":"context-state","status":{"state":%q}
			}}`, test.state))
			evidence, err := validateA2AResult("SendMessage", raw)
			if err != nil {
				t.Fatalf("validateA2AResult: %v", err)
			}
			if evidence.Terminal != test.terminal || evidence.Outcome != test.state {
				t.Fatalf("evidence = %+v, want terminal %v", evidence, test.terminal)
			}
		})
	}
}

func TestProcessorSerializesPendingAndTerminalState(t *testing.T) {
	for _, order := range []string{"working-then-terminal", "terminal-then-working"} {
		t.Run(order, func(t *testing.T) {
			processor, archivePath := testProcessor(t)
			request, err := ParseRequest([]byte(`{
			  "jsonrpc":"2.0","id":"request-1","method":"SendMessage","params":{"message":{"metadata":{
			    "https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":1000}
			  }}}
			}`))
			if err != nil {
				t.Fatalf("ParseRequest: %v", err)
			}
			working := []byte(`{"jsonrpc":"2.0","id":"request-1","result":{"task":{
			  "id":"task-ordered","contextId":"context-ordered",
			  "status":{"state":"TASK_STATE_WORKING"}
			}}}`)
			terminal := []byte(`{"jsonrpc":"2.0","id":"request-1","result":{"task":{
			  "id":"task-ordered","contextId":"context-ordered",
			  "status":{"state":"TASK_STATE_COMPLETED","message":{
			    "messageId":"reply-ordered","role":"ROLE_AGENT","parts":[{"text":"reply"}]
			  }}
			}}}`)

			runWorking := func(expectConflict bool) {
				t.Helper()
				updated, attached, err := processor.TransformResponse(request, working)
				if expectConflict {
					if err == nil || attached || updated != nil {
						t.Fatalf(
							"stale working response = attached %v, err %v, body %s",
							attached,
							err,
							updated,
						)
					}
					return
				}
				if err != nil || attached || string(updated) != string(working) {
					t.Fatalf(
						"working response = attached %v, err %v, body %s",
						attached,
						err,
						updated,
					)
				}
			}
			runTerminal := func() {
				t.Helper()
				if _, attached, err := processor.TransformResponse(request, terminal); err != nil || !attached {
					t.Fatalf("terminal response = attached %v, err %v", attached, err)
				}
			}

			if order == "working-then-terminal" {
				runWorking(false)
				runTerminal()
			} else {
				runTerminal()
				runWorking(true)
			}
			if _, found, err := processor.Pending.Load("task-ordered"); err != nil || found {
				t.Fatalf("pending evidence survived terminal ordering: found %v, err %v", found, err)
			}
			archive, err := os.ReadFile(archivePath)
			if err != nil || strings.Count(string(archive), "\n") != 1 {
				t.Fatalf("archive after ordered responses = %q, err %v", archive, err)
			}
		})
	}
}

func TestProcessorConcurrentPendingAndTerminalStateConverges(t *testing.T) {
	type transformResult struct {
		kind     string
		updated  []byte
		attached bool
		err      error
	}
	for attempt := range 32 {
		processor, archivePath := testProcessor(t)
		request, err := ParseRequest([]byte(`{
		  "jsonrpc":"2.0","id":"request-1","method":"SendMessage","params":{"message":{"metadata":{
		    "https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":1000}
		  }}}
		}`))
		if err != nil {
			t.Fatalf("attempt %d ParseRequest: %v", attempt, err)
		}
		working := []byte(`{"jsonrpc":"2.0","id":"request-1","result":{"task":{
		  "id":"task-concurrent-state","contextId":"context-concurrent-state",
		  "status":{"state":"TASK_STATE_WORKING"}
		}}}`)
		terminal := []byte(`{"jsonrpc":"2.0","id":"request-1","result":{"task":{
		  "id":"task-concurrent-state","contextId":"context-concurrent-state",
		  "status":{"state":"TASK_STATE_COMPLETED"}
		}}}`)

		start := make(chan struct{})
		results := make(chan transformResult, 2)
		run := func(kind string, raw []byte) {
			<-start
			updated, attached, err := processor.TransformResponse(request, raw)
			results <- transformResult{
				kind: kind, updated: updated, attached: attached, err: err,
			}
		}
		go run("working", working)
		go run("terminal", terminal)
		close(start)

		var workingResult, terminalResult transformResult
		for range 2 {
			select {
			case result := <-results:
				if result.kind == "working" {
					workingResult = result
				} else {
					terminalResult = result
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("attempt %d concurrent transitions did not return", attempt)
			}
		}
		if terminalResult.err != nil || !terminalResult.attached {
			t.Fatalf(
				"attempt %d terminal response = attached %v, err %v",
				attempt,
				terminalResult.attached,
				terminalResult.err,
			)
		}
		if workingResult.attached {
			t.Fatalf("attempt %d working response attached a receipt", attempt)
		}
		if workingResult.err == nil {
			if !bytes.Equal(workingResult.updated, working) {
				t.Fatalf(
					"attempt %d working response changed: %s",
					attempt,
					workingResult.updated,
				)
			}
		} else if workingResult.updated != nil {
			t.Fatalf(
				"attempt %d conflicting working response returned a body: %s",
				attempt,
				workingResult.updated,
			)
		}
		if _, found, err := processor.Pending.Load("task-concurrent-state"); err != nil || found {
			t.Fatalf(
				"attempt %d pending evidence survived: found %v, err %v",
				attempt,
				found,
				err,
			)
		}
		archive, err := os.ReadFile(archivePath)
		if err != nil || strings.Count(string(archive), "\n") != 1 {
			t.Fatalf("attempt %d archive = %q, err %v", attempt, archive, err)
		}
	}
}

func TestProcessorRetainsPendingWhenArchivedReceiptCannotAttach(t *testing.T) {
	processor, archivePath := testProcessor(t)
	evidence := pendingEvidence{
		RequestHash: "sha256:" + strings.Repeat("a", 64), TokensReserved: 1000,
	}
	if err := processor.Pending.Save("task-archived", evidence); err != nil {
		t.Fatalf("Save pending evidence: %v", err)
	}
	receipt, err := New(
		processor.AZP,
		"task-archived",
		"context-archived",
		evidence.RequestHash,
		evidence.TokensReserved,
		processor.Now(),
		"TASK_STATE_COMPLETED",
		processor.KeyID,
	)
	if err != nil {
		t.Fatalf("New receipt: %v", err)
	}
	signed, err := Sign(receipt, processor.Key)
	if err != nil {
		t.Fatalf("Sign receipt: %v", err)
	}
	if _, err := processor.Archive.AppendUnique(signed); err != nil {
		t.Fatalf("AppendUnique receipt: %v", err)
	}
	archiveBefore, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("ReadFile archive before response: %v", err)
	}
	request, err := ParseRequest([]byte(
		`{"jsonrpc":"2.0","id":"request-1","method":"GetTask","params":{"id":"task-archived"}}`,
	))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	response := []byte(`{
	  "jsonrpc":"2.0","id":"request-1","result":{
	    "id":"task-archived","contextId":"context-archived",
	    "status":{"state":"TASK_STATE_COMPLETED"},"metadata":"invalid"
	  }
	}`)
	updated, attached, err := processor.TransformResponse(request, response)
	if err == nil || attached || updated != nil {
		t.Fatalf(
			"malformed archived response = attached %v, err %v, body %s",
			attached,
			err,
			updated,
		)
	}
	retained, found, err := processor.Pending.Load("task-archived")
	if err != nil || !found || !reflect.DeepEqual(retained, evidence) {
		t.Fatalf("pending evidence = %+v, found %v, err %v", retained, found, err)
	}
	archiveAfter, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("ReadFile archive after response: %v", err)
	}
	if !bytes.Equal(archiveAfter, archiveBefore) {
		t.Fatal("malformed archived response changed the receipt archive")
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

func TestPendingStoreRejectsDuplicateJSONMembers(t *testing.T) {
	store := &PendingStore{Dir: t.TempDir()}
	path, err := store.path("task-duplicate")
	if err != nil {
		t.Fatalf("pending path: %v", err)
	}
	raw := []byte(fmt.Sprintf(
		`{"requestHash":"sha256:%s","requestHash":"sha256:%s","tokensReserved":10}`,
		strings.Repeat("a", 64),
		strings.Repeat("b", 64),
	))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write duplicate pending evidence: %v", err)
	}
	if _, _, err := store.Load("task-duplicate"); err == nil {
		t.Fatal("Load accepted duplicate pending evidence fields")
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

func assertNoReceiptState(t *testing.T, processor *Processor, archivePath string) {
	t.Helper()
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("invalid response created an archive: %v", err)
	}
	entries, err := os.ReadDir(processor.Pending.Dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read pending evidence directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid response persisted %d pending evidence files", len(entries))
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
	type metadataCarrier struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	var envelope struct {
		Result struct {
			metadataCarrier
			Task *metadataCarrier `json:"task"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	carrier := &envelope.Result.metadataCarrier
	if envelope.Result.Task != nil {
		carrier = envelope.Result.Task
	}
	receiptRaw := carrier.Metadata[ExtensionURI]
	if len(receiptRaw) == 0 {
		t.Fatalf("response has no %s metadata: %s", ExtensionURI, raw)
	}
	signed, err := Parse(receiptRaw)
	if err != nil {
		t.Fatalf("Parse response receipt: %v", err)
	}
	return signed
}
