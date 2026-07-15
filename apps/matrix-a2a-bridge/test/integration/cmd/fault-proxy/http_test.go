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
