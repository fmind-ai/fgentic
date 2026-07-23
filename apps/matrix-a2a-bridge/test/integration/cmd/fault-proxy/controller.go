package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

type faultMode string

const (
	faultPostgresCommit faultMode = "postgres-ledger-commit"
	faultPostgresClaim  faultMode = "postgres-claim"
	faultA2AResponse    faultMode = "a2a-response"
	faultA2ATaskPoll    faultMode = "a2a-task-poll"
	// faultA2ARefuse is a sustained (not one-shot) mode: while armed it fails every A2A request —
	// including the AgentCard resolution GET — fast, modelling a connection-refused backend. It is
	// the model-backend outage drill's fast, deterministic replacement for scaling the backend to
	// zero (an endpointless Service hangs to the request timeout instead of refusing). #466
	faultA2ARefuse      faultMode = "a2a-refuse"
	faultMatrixRequest  faultMode = "matrix-request"
	faultMatrixResponse faultMode = "matrix-response"
	faultMatrixQuestion faultMode = "matrix-question-response"
	faultMatrixProgress faultMode = "matrix-progress-response"
	faultMatrixPin      faultMode = "matrix-pin-response"
)

func (m faultMode) valid() bool {
	switch m {
	case faultPostgresCommit, faultPostgresClaim, faultA2AResponse, faultA2ATaskPoll,
		faultA2ARefuse, faultMatrixRequest, faultMatrixResponse, faultMatrixQuestion,
		faultMatrixProgress, faultMatrixPin:
		return true
	default:
		return false
	}
}

type faultSnapshot struct {
	Mode        faultMode `json:"mode,omitempty"`
	Armed       bool      `json:"armed"`
	Tripped     bool      `json:"tripped"`
	MatchedPath string    `json:"matched_path,omitempty"`
	MatrixPaths []string  `json:"matrix_paths"`
	A2AMethods  []string  `json:"a2a_methods"`
}

type faultController struct {
	mu          sync.Mutex
	mode        faultMode
	armed       bool
	tripped     bool
	matchedPath string
	matrixPaths []string
	a2aMethods  []string
}

func (c *faultController) arm(mode faultMode) error {
	if !mode.valid() {
		return fmt.Errorf("unknown fault mode %q", mode)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.armed {
		return fmt.Errorf("fault mode %q is already armed", c.mode)
	}
	c.mode = mode
	c.armed = true
	c.tripped = false
	c.matchedPath = ""
	c.matrixPaths = nil
	c.a2aMethods = nil
	return nil
}

// refusing reports whether the sustained a2a-refuse mode is armed. Unlike tryTrip it never disarms,
// so every A2A request keeps failing until disarm — the retryable, non-ambiguous card-resolution
// failure the bridge dead-letters after DELEGATION_MAX_ATTEMPTS.
func (c *faultController) refusing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.armed && c.mode == faultA2ARefuse
}

// disarm clears any armed mode so the proxy forwards normally again (drill recovery step).
func (c *faultController) disarm() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = false
	c.mode = ""
	c.tripped = false
}

func (c *faultController) tryTrip(mode faultMode, path string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed || c.mode != mode {
		return false
	}
	c.armed = false
	c.tripped = true
	c.matchedPath = path
	return true
}

func (c *faultController) observeMatrix(path string, modes ...faultMode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	matched := false
	for _, mode := range modes {
		if c.mode == mode {
			matched = true
			break
		}
	}
	if !matched {
		return
	}
	c.matrixPaths = append(c.matrixPaths, path)
}

func (c *faultController) observeA2A(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.a2aMethods = append(c.a2aMethods, method)
}

func (c *faultController) snapshot() faultSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return faultSnapshot{
		Mode:        c.mode,
		Armed:       c.armed,
		Tripped:     c.tripped,
		MatchedPath: c.matchedPath,
		MatrixPaths: append([]string(nil), c.matrixPaths...),
		A2AMethods:  append([]string(nil), c.a2aMethods...),
	}
}

func (c *faultController) controlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /arm/{mode}", func(w http.ResponseWriter, r *http.Request) {
		if err := c.arm(faultMode(r.PathValue("mode"))); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /disarm", func(w http.ResponseWriter, _ *http.Request) {
		c.disarm()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(c.snapshot()); err != nil {
			http.Error(w, "encode fault state", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
