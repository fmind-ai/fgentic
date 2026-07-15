package matrixapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maunium.net/go/mautrix/appservice"
)

const (
	testServerToken = "homeserver-secret"
	testTransaction = "/_matrix/app/v1/transactions/txn-1"
)

type intakeCall struct {
	transactionID string
	bodyHash      [sha256.Size]byte
	body          []byte
}

type recordingAcceptor struct {
	mu          sync.Mutex
	disposition TransactionDisposition
	err         error
	calls       []intakeCall
}

func (a *recordingAcceptor) AcceptTransaction(
	_ context.Context,
	transactionID string,
	bodyHash [sha256.Size]byte,
	body []byte,
) (TransactionDisposition, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, intakeCall{
		transactionID: transactionID,
		bodyHash:      bodyHash,
		body:          bytes.Clone(body),
	})
	return a.disposition, a.err
}

func (a *recordingAcceptor) snapshot() []intakeCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]intakeCall(nil), a.calls...)
}

type trackingReadCloser struct {
	reader   io.Reader
	readErr  error
	closeErr error
	reads    atomic.Int64
	closed   atomic.Bool
}

func (r *trackingReadCloser) Read(data []byte) (int, error) {
	r.reads.Add(1)
	if r.reader != nil {
		count, err := r.reader.Read(data)
		if err != nil || count > 0 {
			return count, err
		}
	}
	return 0, r.readErr
}

func (r *trackingReadCloser) Close() error {
	r.closed.Store(true)
	return r.closeErr
}

func newIntakeAppService() *appservice.AppService {
	as := appservice.Create()
	as.Registration = &appservice.Registration{ServerToken: testServerToken}
	return as
}

func newIntakeRequest(t *testing.T, method, target string, body io.Reader, token string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func responseErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		ErrCode string `json:"errcode"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", response.Body.String(), err)
	}
	return body.ErrCode
}

func TestTransactionIntakeAuthenticationMatchesMautrix(t *testing.T) {
	as := newIntakeAppService()
	acceptor := &recordingAcceptor{disposition: TransactionAccepted}
	var consumerCalls atomic.Int64
	intake, err := NewTransactionIntake(as, acceptor, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		consumerCalls.Add(1)
	}), 1024)
	if err != nil {
		t.Fatalf("NewTransactionIntake: %v", err)
	}

	for _, test := range []struct {
		name  string
		token string
	}{
		{name: "missing"},
		{name: "invalid", token: "wrong-secret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &trackingReadCloser{reader: bytes.NewBufferString(`{"events":[]}`)}
			req := newIntakeRequest(t, http.MethodPut, testTransaction, nil, test.token)
			req.Body = body
			got := httptest.NewRecorder()
			intake.ServeHTTP(got, req)

			want := httptest.NewRecorder()
			baseline := newIntakeRequest(t, http.MethodPut, testTransaction, nil, test.token)
			as.CheckServerToken(want, baseline)
			if got.Code != want.Code || got.Body.String() != want.Body.String() ||
				got.Header().Get("Content-Type") != want.Header().Get("Content-Type") {
				t.Fatalf(
					"auth response = (%d, %q, %q), want (%d, %q, %q)",
					got.Code, got.Header().Get("Content-Type"), got.Body.String(),
					want.Code, want.Header().Get("Content-Type"), want.Body.String(),
				)
			}
			if body.reads.Load() != 0 || body.closed.Load() {
				t.Fatal("unauthenticated request body was read or closed by intake")
			}
		})
	}
	if len(acceptor.snapshot()) != 0 || consumerCalls.Load() != 0 {
		t.Fatal("unauthenticated transaction reached durable acceptance or consumer")
	}
}

func TestTransactionIntakeAcceptedReplayAndConflict(t *testing.T) {
	as := newIntakeAppService()
	var mu sync.Mutex
	accepted := make(map[string][sha256.Size]byte)
	acceptor := TransactionAcceptorFunc(func(
		_ context.Context,
		transactionID string,
		bodyHash [sha256.Size]byte,
		_ []byte,
	) (TransactionDisposition, error) {
		mu.Lock()
		defer mu.Unlock()
		previous, exists := accepted[transactionID]
		if !exists {
			accepted[transactionID] = bodyHash
			return TransactionAccepted, nil
		}
		if previous == bodyHash {
			return TransactionReplay, nil
		}
		return TransactionConflict, nil
	})
	var consumerCalls atomic.Int64
	consumer := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		consumerCalls.Add(1)
		w.Header().Set("X-Consumer", "mautrix")
		w.WriteHeader(http.StatusOK)
	})
	intake, err := NewTransactionIntake(as, acceptor, consumer, 1024)
	if err != nil {
		t.Fatalf("NewTransactionIntake: %v", err)
	}

	request := func(body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		intake.ServeHTTP(response, newIntakeRequest(
			t, http.MethodPut, testTransaction, bytes.NewBufferString(body), testServerToken,
		))
		return response
	}
	first := request(`{"events":[]}`)
	replay := request(`{"events":[]}`)
	conflict := request(`{"events":[],"changed":true}`)

	if first.Code != http.StatusOK || replay.Code != first.Code || replay.Body.String() != first.Body.String() ||
		replay.Header().Get("X-Consumer") != first.Header().Get("X-Consumer") {
		t.Fatalf("accepted response = %#v, replay response = %#v", first.Result(), replay.Result())
	}
	if conflict.Code != http.StatusConflict || responseErrorCode(t, conflict) != "M_INVALID_PARAM" {
		t.Fatalf("conflict response = (%d, %q)", conflict.Code, conflict.Body.String())
	}
	if consumerCalls.Load() != 2 {
		t.Fatalf("consumer calls = %d, want 2", consumerCalls.Load())
	}
}

func TestTransactionIntakeConsumptionBarrierOrdersAcceptedAndReplayWakeups(t *testing.T) {
	type holdProbe struct {
		gate             chan struct{}
		workerPassed     chan struct{}
		consumerReturned atomic.Bool
		released         atomic.Bool
	}

	as := newIntakeAppService()
	var current *holdProbe
	var admissions atomic.Int64
	acceptor := TransactionAcceptorFunc(func(
		_ context.Context,
		_ string,
		_ [sha256.Size]byte,
		_ []byte,
	) (TransactionDisposition, error) {
		probe := current
		go func() {
			<-probe.gate
			close(probe.workerPassed)
		}()
		if admissions.Add(1) == 1 {
			return TransactionAccepted, nil
		}
		return TransactionReplay, nil
	})
	consumer := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probe := current
		if probe.released.Load() {
			t.Fatal("transaction barrier released before consumer started")
		}
		select {
		case <-probe.workerPassed:
			t.Fatal("durable worker crossed transaction barrier during consumer")
		default:
		}
		probe.consumerReturned.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	var wakes atomic.Int64
	intake, err := NewTransactionIntake(
		as,
		acceptor,
		consumer,
		1024,
		WithTransactionConsumptionBarrier(
			func() func() {
				current = &holdProbe{gate: make(chan struct{}), workerPassed: make(chan struct{})}
				probe := current
				var once sync.Once
				return func() {
					once.Do(func() {
						probe.released.Store(true)
						close(probe.gate)
					})
				}
			},
			func() {
				if !current.consumerReturned.Load() || !current.released.Load() {
					t.Fatal("durable wake ran before consumer return and barrier release")
				}
				wakes.Add(1)
			},
		),
	)
	if err != nil {
		t.Fatalf("NewTransactionIntake: %v", err)
	}

	for index := range 2 {
		response := httptest.NewRecorder()
		intake.ServeHTTP(response, newIntakeRequest(
			t, http.MethodPut, testTransaction, bytes.NewBufferString(`{"events":[]}`), testServerToken,
		))
		if response.Code != http.StatusOK {
			t.Fatalf("transaction %d response = (%d, %q)", index, response.Code, response.Body.String())
		}
		select {
		case <-current.workerPassed:
		case <-time.After(time.Second):
			t.Fatalf("transaction %d worker remained behind released barrier", index)
		}
	}
	if wakes.Load() != 2 {
		t.Fatalf("post-consumer wakeups = %d, want accepted + exact replay", wakes.Load())
	}
}

func TestTransactionIntakeConsumptionBarrierReleasesWithoutWakeOnAdmissionFailure(t *testing.T) {
	for _, test := range []struct {
		name        string
		disposition TransactionDisposition
		err         error
		wantStatus  int
	}{
		{name: "conflict", disposition: TransactionConflict, wantStatus: http.StatusConflict},
		{name: "store error", err: errors.New("database unavailable"), wantStatus: http.StatusInternalServerError},
		{name: "unknown disposition", wantStatus: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			as := newIntakeAppService()
			acceptor := &recordingAcceptor{disposition: test.disposition, err: test.err}
			var releases atomic.Int64
			var wakes atomic.Int64
			intake, err := NewTransactionIntake(
				as,
				acceptor,
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					t.Fatal("unaccepted transaction reached consumer")
				}),
				1024,
				WithTransactionConsumptionBarrier(
					func() func() {
						var once sync.Once
						return func() { once.Do(func() { releases.Add(1) }) }
					},
					func() { wakes.Add(1) },
				),
			)
			if err != nil {
				t.Fatalf("NewTransactionIntake: %v", err)
			}

			response := httptest.NewRecorder()
			intake.ServeHTTP(response, newIntakeRequest(
				t, http.MethodPut, testTransaction, bytes.NewBufferString(`{"events":[]}`), testServerToken,
			))
			if response.Code != test.wantStatus {
				t.Fatalf("response status = %d, want %d", response.Code, test.wantStatus)
			}
			if releases.Load() != 1 || wakes.Load() != 0 {
				t.Fatalf("barrier releases/wakeups = (%d, %d), want (1, 0)", releases.Load(), wakes.Load())
			}
		})
	}
}

func TestTransactionIntakePassesExactBodyHashAndRequest(t *testing.T) {
	as := newIntakeAppService()
	body := []byte(" \n{\"events\":[],\"future\":{\"enabled\":true}}\t")
	acceptor := &recordingAcceptor{disposition: TransactionAccepted}
	var consumedBody []byte
	var consumedTransactionID string
	var consumedLength int64
	consumer := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		consumedTransactionID = req.PathValue("txnID")
		consumedLength = req.ContentLength
		var err error
		consumedBody, err = io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read consumer body: %v", err)
		}
		if err := req.Body.Close(); err != nil {
			t.Fatalf("close consumer body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	})
	intake, err := NewTransactionIntake(as, acceptor, consumer, int64(len(body)))
	if err != nil {
		t.Fatalf("NewTransactionIntake: %v", err)
	}
	response := httptest.NewRecorder()
	intake.ServeHTTP(response, newIntakeRequest(
		t, http.MethodPut, "/_matrix/app/v1/transactions/txn%2Fone", bytes.NewReader(body), testServerToken,
	))
	if response.Code != http.StatusOK {
		t.Fatalf("response = (%d, %q)", response.Code, response.Body.String())
	}

	calls := acceptor.snapshot()
	if len(calls) != 1 {
		t.Fatalf("acceptor calls = %d, want 1", len(calls))
	}
	wantHash := sha256.Sum256(body)
	if calls[0].transactionID != "txn/one" || calls[0].bodyHash != wantHash || !bytes.Equal(calls[0].body, body) {
		t.Fatalf("acceptor call = %+v, want exact transaction ID/hash/body", calls[0])
	}
	if consumedTransactionID != "txn/one" || consumedLength != int64(len(body)) || !bytes.Equal(consumedBody, body) {
		t.Fatalf(
			"consumer received transaction=%q length=%d body=%q",
			consumedTransactionID, consumedLength, consumedBody,
		)
	}
}

func TestTransactionIntakeCanUseMautrixRouterAsConsumer(t *testing.T) {
	as := newIntakeAppService()
	var admissions atomic.Int64
	acceptor := TransactionAcceptorFunc(func(
		context.Context,
		string,
		[sha256.Size]byte,
		[]byte,
	) (TransactionDisposition, error) {
		if admissions.Add(1) == 1 {
			return TransactionAccepted, nil
		}
		return TransactionReplay, nil
	})
	intake, err := NewTransactionIntake(as, acceptor, as.Router, 1024)
	if err != nil {
		t.Fatalf("NewTransactionIntake: %v", err)
	}

	var responses []*httptest.ResponseRecorder
	for range 2 {
		response := httptest.NewRecorder()
		intake.ServeHTTP(response, newIntakeRequest(
			t, http.MethodPut, testTransaction, bytes.NewBufferString(`{"events":[]}`), testServerToken,
		))
		responses = append(responses, response)
	}
	for index, response := range responses {
		if response.Code != http.StatusOK || response.Body.String() != "{}" ||
			response.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("response %d = (%d, %q, %q)", index, response.Code, response.Header().Get("Content-Type"), response.Body.String())
		}
	}
}

func TestTransactionIntakeNeverLogsMatrixContentThroughMautrix(t *testing.T) {
	const sentinel = "prompt-must-never-enter-logs"
	as := newIntakeAppService()
	var logs bytes.Buffer
	configureMautrixLogger(as, &logs)
	acceptor := &recordingAcceptor{disposition: TransactionAccepted}
	intake, err := NewTransactionIntake(as, acceptor, as.Router, 4096)
	if err != nil {
		t.Fatalf("NewTransactionIntake: %v", err)
	}
	body := `{"events":[{"event_id":"$sensitive","room_id":"!room:test","sender":"@alice:test","type":"m.room.message","content":{"msgtype":"m.text","body":"` + sentinel + `"}}]}`
	response := httptest.NewRecorder()
	intake.ServeHTTP(response, newIntakeRequest(
		t, http.MethodPut, testTransaction, bytes.NewBufferString(body), testServerToken,
	))
	if response.Code != http.StatusOK {
		t.Fatalf("transaction response = (%d, %q)", response.Code, response.Body.String())
	}
	if strings.Contains(logs.String(), sentinel) || strings.Contains(logs.String(), "$sensitive") {
		t.Fatalf("mautrix transaction log leaked Matrix content: %s", logs.String())
	}
}

func TestTransactionIntakeDelegatesOtherRoutesUntouched(t *testing.T) {
	as := newIntakeAppService()
	as.Router.HandleFunc("GET /custom/{value}", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Delegate", req.PathValue("value"))
		w.WriteHeader(http.StatusTeapot)
	})
	as.Router.HandleFunc("POST /_matrix/app/v1/transactions/{txnID}", func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read delegated body: %v", err)
		}
		w.Header().Set("X-Delegate", string(body))
		w.WriteHeader(http.StatusAccepted)
	})
	acceptor := &recordingAcceptor{disposition: TransactionAccepted}
	var consumerCalls atomic.Int64
	intake, err := NewTransactionIntake(as, acceptor, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		consumerCalls.Add(1)
	}), 1024)
	if err != nil {
		t.Fatalf("NewTransactionIntake: %v", err)
	}

	getResponse := httptest.NewRecorder()
	intake.ServeHTTP(getResponse, newIntakeRequest(t, http.MethodGet, "/custom/value", nil, ""))
	if getResponse.Code != http.StatusTeapot || getResponse.Header().Get("X-Delegate") != "value" {
		t.Fatalf("GET delegate response = (%d, %q)", getResponse.Code, getResponse.Header().Get("X-Delegate"))
	}
	postResponse := httptest.NewRecorder()
	intake.ServeHTTP(postResponse, newIntakeRequest(
		t, http.MethodPost, testTransaction, bytes.NewBufferString("untouched"), "",
	))
	if postResponse.Code != http.StatusAccepted || postResponse.Header().Get("X-Delegate") != "untouched" {
		t.Fatalf("POST delegate response = (%d, %q)", postResponse.Code, postResponse.Header().Get("X-Delegate"))
	}
	nestedResponse := httptest.NewRecorder()
	intake.ServeHTTP(nestedResponse, newIntakeRequest(
		t, http.MethodPut, testTransaction+"/extra", bytes.NewBufferString(`{"events":[]}`), "",
	))
	if nestedResponse.Code != http.StatusNotFound {
		t.Fatalf("nested transaction path status = %d, want delegated 404", nestedResponse.Code)
	}
	if len(acceptor.snapshot()) != 0 || consumerCalls.Load() != 0 {
		t.Fatal("non-PUT transaction route reached intake")
	}
}

func TestTransactionIntakeRejectsInvalidBodiesBeforeAdmission(t *testing.T) {
	tests := []struct {
		name       string
		body       func() io.ReadCloser
		maxBody    int64
		wantStatus int
		wantCode   string
	}{
		{
			name:       "nil",
			body:       func() io.ReadCloser { return nil },
			maxBody:    1024,
			wantStatus: http.StatusBadRequest,
			wantCode:   "M_NOT_JSON",
		},
		{
			name:       "empty",
			body:       func() io.ReadCloser { return http.NoBody },
			maxBody:    1024,
			wantStatus: http.StatusBadRequest,
			wantCode:   "M_NOT_JSON",
		},
		{
			name:       "invalid JSON",
			body:       func() io.ReadCloser { return io.NopCloser(bytes.NewBufferString(`{"events":`)) },
			maxBody:    1024,
			wantStatus: http.StatusBadRequest,
			wantCode:   "M_BAD_JSON",
		},
		{
			name:       "wrong JSON shape",
			body:       func() io.ReadCloser { return io.NopCloser(bytes.NewBufferString(`[]`)) },
			maxBody:    1024,
			wantStatus: http.StatusBadRequest,
			wantCode:   "M_BAD_JSON",
		},
		{
			name:       "oversize",
			body:       func() io.ReadCloser { return io.NopCloser(bytes.NewBufferString(`{"events":[]}`)) },
			maxBody:    int64(len(`{"events":[]}`) - 1),
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   "M_TOO_LARGE",
		},
		{
			name: "read failure",
			body: func() io.ReadCloser {
				return &trackingReadCloser{readErr: errors.New("read failed")}
			},
			maxBody:    1024,
			wantStatus: http.StatusBadRequest,
			wantCode:   "M_NOT_JSON",
		},
		{
			name: "close failure",
			body: func() io.ReadCloser {
				return &trackingReadCloser{
					reader:   bytes.NewBufferString(`{"events":[]}`),
					readErr:  io.EOF,
					closeErr: errors.New("close failed"),
				}
			},
			maxBody:    1024,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "M_UNKNOWN",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			as := newIntakeAppService()
			acceptor := &recordingAcceptor{disposition: TransactionAccepted}
			var consumerCalls atomic.Int64
			intake, err := NewTransactionIntake(as, acceptor, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				consumerCalls.Add(1)
			}), test.maxBody)
			if err != nil {
				t.Fatalf("NewTransactionIntake: %v", err)
			}
			req := newIntakeRequest(t, http.MethodPut, testTransaction, nil, testServerToken)
			req.Body = test.body()
			response := httptest.NewRecorder()
			intake.ServeHTTP(response, req)
			if response.Code != test.wantStatus || responseErrorCode(t, response) != test.wantCode {
				t.Fatalf("response = (%d, %q)", response.Code, response.Body.String())
			}
			if len(acceptor.snapshot()) != 0 || consumerCalls.Load() != 0 {
				t.Fatal("invalid body reached acceptance or consumer")
			}
		})
	}
}

func TestTransactionIntakeFailsClosedOnAdmissionErrors(t *testing.T) {
	for _, test := range []struct {
		name        string
		disposition TransactionDisposition
		err         error
	}{
		{name: "store error", disposition: TransactionAccepted, err: errors.New("database unavailable")},
		{name: "unknown disposition"},
	} {
		t.Run(test.name, func(t *testing.T) {
			as := newIntakeAppService()
			acceptor := &recordingAcceptor{disposition: test.disposition, err: test.err}
			var consumerCalls atomic.Int64
			intake, err := NewTransactionIntake(as, acceptor, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				consumerCalls.Add(1)
			}), 1024)
			if err != nil {
				t.Fatalf("NewTransactionIntake: %v", err)
			}
			response := httptest.NewRecorder()
			intake.ServeHTTP(response, newIntakeRequest(
				t, http.MethodPut, testTransaction, bytes.NewBufferString(`{"events":[]}`), testServerToken,
			))
			if response.Code != http.StatusInternalServerError || responseErrorCode(t, response) != "M_UNKNOWN" {
				t.Fatalf("response = (%d, %q)", response.Code, response.Body.String())
			}
			if consumerCalls.Load() != 0 {
				t.Fatal("failed admission reached consumer")
			}
		})
	}
}

func TestTransactionIntakeSerializesAcceptanceAndConsumption(t *testing.T) {
	as := newIntakeAppService()
	accepted := make(chan string, 2)
	acceptor := TransactionAcceptorFunc(func(
		_ context.Context,
		transactionID string,
		_ [sha256.Size]byte,
		_ []byte,
	) (TransactionDisposition, error) {
		accepted <- transactionID
		return TransactionAccepted, nil
	})
	firstConsumerStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	consumer := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.PathValue("txnID") == "first" {
			close(firstConsumerStarted)
			<-releaseFirst
		}
		w.WriteHeader(http.StatusOK)
	})
	intake, err := NewTransactionIntake(as, acceptor, consumer, 1024)
	if err != nil {
		t.Fatalf("NewTransactionIntake: %v", err)
	}

	requestDone := make(chan struct{}, 2)
	serve := func(transactionID string) {
		response := httptest.NewRecorder()
		intake.ServeHTTP(response, newIntakeRequest(
			t,
			http.MethodPut,
			"/_matrix/app/v1/transactions/"+transactionID,
			bytes.NewBufferString(`{"events":[]}`),
			testServerToken,
		))
		requestDone <- struct{}{}
	}
	go serve("first")
	if got := <-accepted; got != "first" {
		t.Fatalf("first accepted transaction = %q", got)
	}
	<-firstConsumerStarted
	go serve("second")
	select {
	case got := <-accepted:
		close(releaseFirst)
		t.Fatalf("transaction %q was accepted while first consumer was active", got)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	if got := <-accepted; got != "second" {
		t.Fatalf("second accepted transaction = %q", got)
	}
	<-requestDone
	<-requestDone
}

func TestNewTransactionIntakeValidatesDependencies(t *testing.T) {
	as := newIntakeAppService()
	acceptor := &recordingAcceptor{disposition: TransactionAccepted}
	consumer := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	tests := []struct {
		name       string
		appservice *appservice.AppService
		acceptor   TransactionAcceptor
		consumer   http.Handler
		maxBody    int64
	}{
		{name: "nil appservice", acceptor: acceptor, consumer: consumer, maxBody: 1},
		{name: "nil registration", appservice: appservice.Create(), acceptor: acceptor, consumer: consumer, maxBody: 1},
		{
			name:       "nil router",
			appservice: &appservice.AppService{Registration: &appservice.Registration{}},
			acceptor:   acceptor,
			consumer:   consumer,
			maxBody:    1,
		},
		{name: "nil acceptor", appservice: as, consumer: consumer, maxBody: 1},
		{name: "nil consumer", appservice: as, acceptor: acceptor, maxBody: 1},
		{name: "zero limit", appservice: as, acceptor: acceptor, consumer: consumer},
		{name: "negative limit", appservice: as, acceptor: acceptor, consumer: consumer, maxBody: -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if intake, err := NewTransactionIntake(
				test.appservice, test.acceptor, test.consumer, test.maxBody,
			); err == nil || intake != nil {
				t.Fatalf("NewTransactionIntake() = (%v, %v), want (nil, error)", intake, err)
			}
		})
	}
	for _, test := range []struct {
		name   string
		option TransactionIntakeOption
	}{
		{name: "nil option"},
		{name: "nil barrier acquire", option: WithTransactionConsumptionBarrier(nil, func() {})},
		{name: "nil post-consumer callback", option: WithTransactionConsumptionBarrier(func() func() {
			return func() {}
		}, nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if intake, err := NewTransactionIntake(as, acceptor, consumer, 1, test.option); err == nil || intake != nil {
				t.Fatalf("NewTransactionIntake() = (%v, %v), want (nil, error)", intake, err)
			}
		})
	}
}

func TestTransactionAcceptorFunc(t *testing.T) {
	wantHash := sha256.Sum256([]byte("body"))
	wantBody := []byte("body")
	called := false
	acceptor := TransactionAcceptorFunc(func(
		ctx context.Context,
		transactionID string,
		bodyHash [sha256.Size]byte,
		body []byte,
	) (TransactionDisposition, error) {
		called = true
		if ctx != t.Context() || transactionID != "txn" || bodyHash != wantHash || !reflect.DeepEqual(body, wantBody) {
			t.Fatalf("adapter arguments = (%v, %q, %x, %q)", ctx, transactionID, bodyHash, body)
		}
		return TransactionReplay, nil
	})
	got, err := acceptor.AcceptTransaction(t.Context(), "txn", wantHash, wantBody)
	if err != nil || got != TransactionReplay || !called {
		t.Fatalf("AcceptTransaction() = (%d, %v), called = %t", got, err, called)
	}
}
