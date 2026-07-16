package usagereceipt

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestExternalProcessorInjectsReceiptOverBufferedProtocol(t *testing.T) {
	processor, _ := testProcessor(t)
	stream := newExternalProcessorStream(t, processor)
	request := []byte(`{"jsonrpc":"2.0","id":"request-1","method":"SendMessage","params":{"message":{"metadata":{"https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":25}}}}}`)
	if err := stream.Send(processingBody(true, request)); err != nil {
		t.Fatalf("Send request body: %v", err)
	}
	if response, err := stream.Recv(); err != nil || response.GetRequestBody() == nil {
		t.Fatalf("Recv request continuation: response %v, err %v", response, err)
	}
	upstream := []byte(`{
  "jsonrpc":"2.0",
  "id":"request-1",
  "result":{"task":{
    "id":"task-1",
    "contextId":"context-1",
    "status":{"state":"TASK_STATE_COMPLETED","message":{
      "messageId":"reply-1",
      "role":"ROLE_AGENT",
      "parts":[{"text":"reply"}]
    }}
  }}
}`)
	if err := stream.Send(processingBody(false, upstream)); err != nil {
		t.Fatalf("Send response body: %v", err)
	}
	response, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv response mutation: %v", err)
	}
	mutation := response.GetResponseBody().GetResponse().GetBodyMutation().GetBody()
	if len(mutation) == 0 {
		t.Fatal("external processor returned no response-body mutation")
	}
	var envelope struct {
		Result a2a.StreamResponse `json:"result"`
	}
	if err := json.Unmarshal(mutation, &envelope); err != nil {
		t.Fatalf("decode external-processor response through a2a-go: %v", err)
	}
	task, ok := envelope.Result.Event.(*a2a.Task)
	if !ok || task.Metadata[ExtensionURI] == nil {
		t.Fatalf("external processor omitted native A2A extension data: %#v", envelope.Result.Event)
	}
	signed := receiptFromResponse(t, mutation)
	if err := Verify(signed, &processor.Key.PublicKey, processor.KeyID); err != nil {
		t.Fatalf("Verify mutated response receipt: %v", err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
}

func TestExternalProcessorDoesNotExposeProviderResponseContent(t *testing.T) {
	processor, _ := testProcessor(t)
	stream := newExternalProcessorStream(t, processor)
	request := []byte(`{"jsonrpc":"2.0","id":"request-1","method":"SendMessage","params":{"message":{"metadata":{"https://fgentic.fmind.ai/a2a/extensions/token-budget/v1":{"maxTokens":25}}}}}`)
	if err := stream.Send(processingBody(true, request)); err != nil {
		t.Fatalf("Send request body: %v", err)
	}
	if response, err := stream.Recv(); err != nil || response.GetRequestBody() == nil {
		t.Fatalf("Recv request continuation: response %v, err %v", response, err)
	}
	const marker = "private-provider-response-marker"
	upstream := []byte(`{
  "jsonrpc":"2.0",
  "id":"request-1",
  "result":{"task":{
    "id":"task-1",
    "contextId":"context-1",
    "status":{
      "state":"TASK_STATE_COMPLETED",
      "timestamp":"private-provider-response-marker"
    }
  }}
}`)
	if err := stream.Send(processingBody(false, upstream)); err != nil {
		t.Fatalf("Send response body: %v", err)
	}
	_, err := stream.Recv()
	if status.Code(err) != codes.Internal {
		t.Fatalf("Recv malformed response code = %v, err %v", status.Code(err), err)
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("external processor exposed provider response content: %v", err)
	}
	if got := status.Convert(err).Message(); got != "produce usage receipt" {
		t.Fatalf("external processor error = %q", got)
	}
}

func newExternalProcessorStream(
	t *testing.T,
	processor *Processor,
) extprocv3.ExternalProcessor_ProcessClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(server, &ExternalProcessor{Processor: processor})
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		if err := <-serveErr; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("Serve: %v", err)
		}
	})

	connection, err := grpc.NewClient(
		"passthrough:///usage-receipt",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() {
		if err := connection.Close(); err != nil {
			t.Errorf("Close client: %v", err)
		}
	})
	stream, err := extprocv3.NewExternalProcessorClient(connection).Process(context.Background())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return stream
}

func processingBody(request bool, body []byte) *extprocv3.ProcessingRequest {
	httpBody := &extprocv3.HttpBody{Body: body, EndOfStream: true}
	if request {
		return &extprocv3.ProcessingRequest{
			Request: &extprocv3.ProcessingRequest_RequestBody{RequestBody: httpBody},
		}
	}
	return &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_ResponseBody{ResponseBody: httpBody},
	}
}
