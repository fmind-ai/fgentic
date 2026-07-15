package matrixapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
)

// TransactionDisposition is the durable admission result for one appservice transaction.
type TransactionDisposition uint8

const (
	// TransactionAccepted means the exact transaction and its planned work were committed durably.
	TransactionAccepted TransactionDisposition = iota + 1
	// TransactionReplay means the transaction ID was already committed with the same body hash.
	TransactionReplay
	// TransactionConflict means the transaction ID was already committed with a different body hash.
	TransactionConflict
)

// TransactionAcceptor is the state-facing pre-ACK seam. Implementations must atomically commit the
// transaction and all work derived from body before returning TransactionAccepted. body is exact,
// read-only request data and must not be retained or modified after AcceptTransaction returns.
type TransactionAcceptor interface {
	AcceptTransaction(
		ctx context.Context,
		transactionID string,
		bodyHash [sha256.Size]byte,
		body []byte,
	) (TransactionDisposition, error)
}

// TransactionAcceptorFunc adapts a function to TransactionAcceptor.
type TransactionAcceptorFunc func(
	ctx context.Context,
	transactionID string,
	bodyHash [sha256.Size]byte,
	body []byte,
) (TransactionDisposition, error)

// AcceptTransaction implements TransactionAcceptor.
func (f TransactionAcceptorFunc) AcceptTransaction(
	ctx context.Context,
	transactionID string,
	bodyHash [sha256.Size]byte,
	body []byte,
) (TransactionDisposition, error) {
	return f(ctx, transactionID, bodyHash, body)
}

// TransactionIntake wraps mautrix's router with durable, hash-bound transaction admission. Only
// the stable appservice transaction PUT route is intercepted; every other route goes directly to
// the original router. Accepted intake is serialized through the downstream consumer so mautrix's
// per-transaction event ordering cannot be interleaved by concurrent HTTP handlers.
type TransactionIntake struct {
	appservice  *appservice.AppService
	acceptor    TransactionAcceptor
	consumer    http.Handler
	maxBodySize int64
	router      *http.ServeMux
	mu          sync.Mutex
	barrier     transactionConsumptionBarrier
}

// TransactionIntakeOption configures optional transaction lifecycle behavior.
type TransactionIntakeOption func(*TransactionIntake) error

type transactionConsumptionBarrier struct {
	acquire       func() func()
	afterConsumed func()
}

// WithTransactionConsumptionBarrier prevents downstream durable work from starting between its
// pre-ACK commit and mautrix consuming the same transaction. acquire must return an idempotent
// release function. afterConsumed runs only after the consumer returns for an accepted transaction
// or exact replay, and is the appropriate place to wake a durable worker.
func WithTransactionConsumptionBarrier(
	acquire func() func(),
	afterConsumed func(),
) TransactionIntakeOption {
	return func(intake *TransactionIntake) error {
		if acquire == nil {
			return fmt.Errorf("configure transaction consumption barrier: acquire callback is nil")
		}
		if afterConsumed == nil {
			return fmt.Errorf("configure transaction consumption barrier: post-consumer callback is nil")
		}
		intake.barrier = transactionConsumptionBarrier{
			acquire:       acquire,
			afterConsumed: afterConsumed,
		}
		return nil
	}
}

// NewTransactionIntake constructs a pre-ACK transaction handler. consumer receives a cloned
// authenticated request whose body contains the exact bytes admitted durably; pass the original
// AppService.Router to retain mautrix parsing, state-event handling, transaction deduplication, and
// response semantics. maxBodySize must be positive.
func NewTransactionIntake(
	as *appservice.AppService,
	acceptor TransactionAcceptor,
	consumer http.Handler,
	maxBodySize int64,
	options ...TransactionIntakeOption,
) (*TransactionIntake, error) {
	if as == nil {
		return nil, fmt.Errorf("create transaction intake: appservice is nil")
	}
	if as.Registration == nil {
		return nil, fmt.Errorf("create transaction intake: appservice registration is nil")
	}
	if as.Router == nil {
		return nil, fmt.Errorf("create transaction intake: appservice router is nil")
	}
	if acceptor == nil {
		return nil, fmt.Errorf("create transaction intake: acceptor is nil")
	}
	if consumer == nil {
		return nil, fmt.Errorf("create transaction intake: consumer is nil")
	}
	if maxBodySize <= 0 {
		return nil, fmt.Errorf("create transaction intake: max body size must be positive")
	}

	intake := &TransactionIntake{
		appservice:  as,
		acceptor:    acceptor,
		consumer:    consumer,
		maxBodySize: maxBodySize,
		router:      http.NewServeMux(),
	}
	for index, option := range options {
		if option == nil {
			return nil, fmt.Errorf("create transaction intake: option %d is nil", index)
		}
		if err := option(intake); err != nil {
			return nil, fmt.Errorf("create transaction intake: option %d: %w", index, err)
		}
	}
	intake.router.HandleFunc("PUT /_matrix/app/v1/transactions/{txnID}", intake.putTransaction)
	intake.router.Handle("/", as.Router)
	return intake, nil
}

// ServeHTTP implements http.Handler.
func (i *TransactionIntake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	i.router.ServeHTTP(w, r)
}

func (i *TransactionIntake) putTransaction(w http.ResponseWriter, r *http.Request) {
	if !i.appservice.CheckServerToken(w, r) {
		return
	}

	transactionID := r.PathValue("txnID")
	if transactionID == "" {
		mautrix.MInvalidParam.WithMessage("Missing transaction ID").Write(w)
		return
	}
	// Take the single admission slot before allocating the bounded body. This keeps concurrent
	// authenticated homeserver requests within one maxBodySize buffer, which matters under the
	// bridge pod's constrained memory limit as well as for downstream event ordering.
	i.mu.Lock()
	defer i.mu.Unlock()
	body, ok := i.readBody(w, r)
	if !ok {
		return
	}
	// Match mautrix's concrete Transaction decode before creating durable state. This makes the
	// subsequent consumer decode deterministic and prevents malformed bytes from claiming a txn ID.
	var transaction appservice.Transaction
	if err := json.Unmarshal(body, &transaction); err != nil {
		mautrix.MBadJSON.WithMessage("Failed to parse transaction content").Write(w)
		return
	}

	bodyHash := sha256.Sum256(body)
	releaseBarrier := func() {}
	if i.barrier.acquire != nil {
		releaseBarrier = i.barrier.acquire()
		if releaseBarrier == nil {
			mautrix.MUnknown.WithMessage("Failed to acquire transaction consumption barrier").Write(w)
			return
		}
	}
	barrierReleased := false
	defer func() {
		if !barrierReleased {
			releaseBarrier()
		}
	}()
	disposition, err := i.acceptor.AcceptTransaction(r.Context(), transactionID, bodyHash, body)
	if err != nil {
		mautrix.MUnknown.WithMessage("Failed to durably accept transaction").Write(w)
		return
	}
	switch disposition {
	case TransactionAccepted, TransactionReplay:
		consumerRequest := r.Clone(r.Context())
		consumerRequest.Body = io.NopCloser(bytes.NewReader(body))
		consumerRequest.ContentLength = int64(len(body))
		consumerRequest.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		consumerRequest.SetPathValue("txnID", transactionID)
		i.consumer.ServeHTTP(w, consumerRequest)
		releaseBarrier()
		barrierReleased = true
		if i.barrier.afterConsumed != nil {
			i.barrier.afterConsumed()
		}
	case TransactionConflict:
		mautrix.MInvalidParam.
			WithStatus(http.StatusConflict).
			WithMessage("Transaction ID was already accepted with different content").
			Write(w)
	default:
		mautrix.MUnknown.WithMessage("Invalid durable transaction admission result").Write(w)
	}
}

func (i *TransactionIntake) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		mautrix.MNotJSON.WithMessage("Failed to read response body").Write(w)
		return nil, false
	}
	reader := http.MaxBytesReader(w, r.Body, i.maxBodySize)
	body, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(readErr, &tooLarge) {
			mautrix.MTooLarge.WithMessage("Transaction body is too large").Write(w)
		} else {
			mautrix.MNotJSON.WithMessage("Failed to read response body").Write(w)
		}
		return nil, false
	}
	if closeErr != nil {
		mautrix.MUnknown.WithMessage("Failed to close transaction body").Write(w)
		return nil, false
	}
	if len(body) == 0 {
		mautrix.MNotJSON.WithMessage("Failed to read response body").Write(w)
		return nil, false
	}
	return body, true
}
