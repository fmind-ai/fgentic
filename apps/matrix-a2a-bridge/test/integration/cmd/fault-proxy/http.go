package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

type faultTransport struct {
	kind       string
	controller *faultController
	base       http.RoundTripper
}

func (t faultTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch t.kind {
	case "matrix":
		return t.matrixRoundTrip(req)
	case "a2a":
		return t.a2aRoundTrip(req)
	default:
		return nil, fmt.Errorf("unknown HTTP fault proxy kind %q", t.kind)
	}
}

func (t faultTransport) matrixRoundTrip(req *http.Request) (*http.Response, error) {
	isSend := req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/send/")
	controlMode, err := matrixControlMode(req)
	if err != nil {
		return nil, err
	}
	if isSend {
		t.controller.observeMatrix(req.URL.Path, faultMatrixRequest, faultMatrixResponse)
	}
	if controlMode != "" {
		t.controller.observeMatrix(req.URL.Path, controlMode)
	}
	if isSend {
		if t.controller.tryTrip(faultMatrixRequest, req.URL.Path) {
			return waitForRequestDeath(req.Context(), faultMatrixRequest)
		}
	}
	response, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return response, nil
	}
	responseMode := faultMode("")
	if isSend && t.controller.tryTrip(faultMatrixResponse, req.URL.Path) {
		responseMode = faultMatrixResponse
	} else if controlMode != "" && t.controller.tryTrip(controlMode, req.URL.Path) {
		responseMode = controlMode
	}
	if responseMode == "" {
		return response, nil
	}
	if err := drainAndClose(response.Body); err != nil {
		return nil, fmt.Errorf("read accepted Matrix response before fault: %w", err)
	}
	return waitForRequestDeath(req.Context(), responseMode)
}

func (t faultTransport) a2aRoundTrip(req *http.Request) (*http.Response, error) {
	// Sustained refusal fails every A2A request — including the AgentCard resolution GET — before it
	// reaches the upstream, so the bridge's cold-cache card resolution fails fast and non-ambiguously
	// (no send is ever attempted), driving the bounded errorA2APreflightRetry dead-letter path. #466
	if t.controller.refusing() {
		return nil, fmt.Errorf("%s injected connection failure", faultA2ARefuse)
	}
	method, err := a2aMethod(req)
	if err != nil {
		return nil, err
	}
	if method != "" {
		t.controller.observeA2A(method)
	}
	if isGetTask(method) && t.controller.tryTrip(faultA2ATaskPoll, req.URL.Path) {
		return waitForRequestDeath(req.Context(), faultA2ATaskPoll)
	}
	response, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if !isAmbiguousA2AMutation(method) || !t.controller.tryTrip(faultA2AResponse, req.URL.Path) {
		return response, nil
	}
	if err := drainAndClose(response.Body); err != nil {
		return nil, fmt.Errorf("read accepted A2A response before fault: %w", err)
	}
	return waitForRequestDeath(req.Context(), faultA2AResponse)
}

func waitForRequestDeath(ctx context.Context, mode faultMode) (*http.Response, error) {
	<-ctx.Done()
	return nil, fmt.Errorf("%s injected after request boundary: %w", mode, context.Cause(ctx))
}

func drainAndClose(body io.ReadCloser) error {
	_, readErr := io.Copy(io.Discard, io.LimitReader(body, 2<<20))
	closeErr := body.Close()
	if readErr != nil {
		return readErr
	}
	return closeErr
}

func a2aMethod(req *http.Request) (string, error) {
	if req.Method != http.MethodPost || req.Body == nil {
		return "", nil
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 2<<20))
	if err != nil {
		return "", fmt.Errorf("read A2A request for fault routing: %w", err)
	}
	if err := req.Body.Close(); err != nil {
		return "", fmt.Errorf("close A2A request body for fault routing: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	var envelope struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", nil
	}
	return envelope.Method, nil
}

func isSendMessage(method string) bool {
	return method == "SendMessage"
}

func isAmbiguousA2AMutation(method string) bool {
	return isSendMessage(method) || method == "CancelTask"
}

func isGetTask(method string) bool {
	return method == "GetTask"
}

func matrixControlMode(req *http.Request) (faultMode, error) {
	if req.Method != http.MethodPut {
		return "", nil
	}
	if strings.Contains(req.URL.Path, "/state/m.room.pinned_events/") {
		return faultMatrixPin, nil
	}
	if !strings.Contains(req.URL.Path, "/send/") || req.Body == nil {
		return "", nil
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 2<<20))
	if err != nil {
		return "", fmt.Errorf("read Matrix request for fault routing: %w", err)
	}
	if err := req.Body.Close(); err != nil {
		return "", fmt.Errorf("close Matrix request body for fault routing: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	var content struct {
		RelatesTo struct {
			RelType string `json:"rel_type"`
		} `json:"m.relates_to"`
	}
	if err := json.Unmarshal(body, &content); err != nil {
		return "", nil
	}
	switch content.RelatesTo.RelType {
	case "m.replace":
		return faultMatrixQuestion, nil
	case "m.thread":
		return faultMatrixProgress, nil
	default:
		return "", nil
	}
}

func newHTTPProxy(targetRaw, kind string, controller *faultController) (http.Handler, error) {
	target, err := url.Parse(targetRaw)
	if err != nil {
		return nil, fmt.Errorf("parse %s upstream %q: %w", kind, targetRaw, err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = faultTransport{kind: kind, controller: controller, base: http.DefaultTransport}
	return proxy, nil
}
