package usagereceipt

import (
	"errors"
	"fmt"
	"io"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ExternalProcessor adapts Processor to Envoy's bidirectional external-processing protocol.
type ExternalProcessor struct {
	extprocv3.UnimplementedExternalProcessorServer
	Processor *Processor
}

// Process receives buffered A2A request/response bodies and mutates only terminal responses.
func (s *ExternalProcessor) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	if s == nil || s.Processor == nil {
		return status.Error(codes.FailedPrecondition, "usage receipt processor is unavailable")
	}
	var evidence requestEvidence
	for {
		request, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("receive external-processing message: %w", err)
		}
		switch {
		case request.GetRequestHeaders() != nil:
			if err := stream.Send(headersContinue(true)); err != nil {
				return fmt.Errorf("continue request headers: %w", err)
			}
		case request.GetRequestBody() != nil:
			body := request.GetRequestBody()
			if !body.GetEndOfStream() {
				return status.Error(codes.InvalidArgument, "usage receipt request body must be buffered")
			}
			evidence, err = ParseRequest(body.GetBody())
			if err != nil {
				return status.Error(codes.InvalidArgument, "invalid A2A request")
			}
			if err := stream.Send(bodyContinue(true, nil)); err != nil {
				return fmt.Errorf("continue request body: %w", err)
			}
		case request.GetResponseHeaders() != nil:
			if err := stream.Send(headersContinue(false)); err != nil {
				return fmt.Errorf("continue response headers: %w", err)
			}
		case request.GetResponseBody() != nil:
			body := request.GetResponseBody()
			if !body.GetEndOfStream() {
				return status.Error(codes.InvalidArgument, "usage receipt response body must be buffered")
			}
			updated, attached, err := s.Processor.TransformResponse(evidence, body.GetBody())
			if err != nil {
				return status.Error(codes.Internal, "produce usage receipt")
			}
			if !attached {
				updated = nil
			}
			if err := stream.Send(bodyContinue(false, updated)); err != nil {
				return fmt.Errorf("continue response body: %w", err)
			}
		default:
			return status.Error(codes.InvalidArgument, "unexpected external-processing phase")
		}
	}
}

func headersContinue(request bool) *extprocv3.ProcessingResponse {
	response := &extprocv3.HeadersResponse{Response: &extprocv3.CommonResponse{}}
	if request {
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestHeaders{RequestHeaders: response},
		}
	}
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseHeaders{ResponseHeaders: response},
	}
}

func bodyContinue(request bool, replacement []byte) *extprocv3.ProcessingResponse {
	common := &extprocv3.CommonResponse{}
	if replacement != nil {
		common.BodyMutation = &extprocv3.BodyMutation{
			Mutation: &extprocv3.BodyMutation_Body{Body: replacement},
		}
	}
	response := &extprocv3.BodyResponse{Response: common}
	if request {
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestBody{RequestBody: response},
		}
	}
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseBody{ResponseBody: response},
	}
}
