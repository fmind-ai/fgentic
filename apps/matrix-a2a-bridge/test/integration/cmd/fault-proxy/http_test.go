package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestMatrixResponseFaultWaitsUntilRequestDies(t *testing.T) {
	controller := &faultController{}
	if err := controller.arm(faultMatrixResponse); err != nil {
		t.Fatal(err)
	}
	transport := faultTransport{
		kind:       "matrix",
		controller: controller,
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"event_id":"$one"}`)),
			}, nil
		}),
	}
	ctx, cancel := context.WithCancel(t.Context())
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://matrix/_matrix/client/v3/rooms/r/send/m.room.message/txn", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := transport.RoundTrip(request)
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for !controller.snapshot().Tripped && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !controller.snapshot().Tripped {
		t.Fatal("Matrix response fault did not trip")
	}
	cancel()
	if err := <-done; err == nil || !strings.Contains(err.Error(), string(faultMatrixResponse)) {
		t.Fatalf("RoundTrip error = %v", err)
	}
}

func TestMatrixResponseFaultDoesNotTripRejectedRequest(t *testing.T) {
	controller := &faultController{}
	if err := controller.arm(faultMatrixPin); err != nil {
		t.Fatal(err)
	}
	transport := faultTransport{
		kind:       "matrix",
		controller: controller,
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader(`{"errcode":"M_FORBIDDEN"}`)),
			}, nil
		}),
	}
	request, err := http.NewRequest(
		http.MethodPut,
		"http://matrix/_matrix/client/v3/rooms/r/state/m.room.pinned_events/",
		strings.NewReader(`{"pinned":["$one"]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	if controller.snapshot().Tripped {
		t.Fatal("Matrix response fault tripped for a rejected request")
	}
}

func TestA2AMethodRestoresRequestBody(t *testing.T) {
	want := []byte(`{"jsonrpc":"2.0","method":"SendMessage"}`)
	request, err := http.NewRequest(http.MethodPost, "http://a2a/", bytes.NewReader(want))
	if err != nil {
		t.Fatal(err)
	}
	method, err := a2aMethod(request)
	if err != nil {
		t.Fatal(err)
	}
	if method != "SendMessage" {
		t.Fatalf("method = %q", method)
	}
	got, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("restored body = %q, want %q", got, want)
	}
}

func TestMatrixControlModeClassifiesDurableProjections(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
		want faultMode
	}{
		{name: "question", path: "/send/m.room.message/txn", body: `{"m.relates_to":{"rel_type":"m.replace"}}`, want: faultMatrixQuestion},
		{name: "progress", path: "/send/m.room.message/txn", body: `{"m.relates_to":{"rel_type":"m.thread"}}`, want: faultMatrixProgress},
		{name: "pin", path: "/state/m.room.pinned_events/", body: `{"pinned":["$one"]}`, want: faultMatrixPin},
		{name: "ordinary", path: "/send/m.room.message/txn", body: `{"body":"working"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPut, "http://matrix"+tt.path, strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			got, err := matrixControlMode(request)
			if err != nil || got != tt.want {
				t.Fatalf("matrixControlMode = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestCancelTaskUsesA2AResponseBoundary(t *testing.T) {
	if !isAmbiguousA2AMutation("CancelTask") {
		t.Fatal("CancelTask was not classified as an ambiguous A2A mutation")
	}
}
