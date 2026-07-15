package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
)

const maxPostgresMessageBytes = 16 << 20

type postgresConnState struct {
	mu                 sync.Mutex
	statements         map[string]string
	ledgerTransaction  bool
	trapCommitResponse bool
}

func newPostgresConnState() *postgresConnState {
	return &postgresConnState{statements: make(map[string]string)}
}

func (s *postgresConnState) rememberStatement(name, query string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statements[name] = query
}

func (s *postgresConnState) statement(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statements[name]
}

func (s *postgresConnState) observeQuery(query string) {
	normalized := normalizeSQL(query)
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.Contains(normalized, "insert into bridge_appservice_transactions") {
		s.ledgerTransaction = true
	}
	if normalized == "commit" || strings.HasPrefix(normalized, "commit ") {
		s.trapCommitResponse = s.ledgerTransaction
		s.ledgerTransaction = false
	}
	if normalized == "rollback" || strings.HasPrefix(normalized, "rollback ") {
		s.ledgerTransaction = false
		s.trapCommitResponse = false
	}
}

func (s *postgresConnState) takeCommitTrap() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	trap := s.trapCommitResponse
	s.trapCommitResponse = false
	return trap
}

func normalizeSQL(query string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(query))), " ")
}

func isClaimQuery(query string) bool {
	normalized := normalizeSQL(query)
	return strings.Contains(normalized, "bridge_delegations") &&
		strings.Contains(normalized, "for update of candidate_job skip locked")
}

type postgresProxy struct {
	controller *faultController
	upstream   string
}

func (p postgresProxy) serve(ctx context.Context, listener net.Listener) error {
	for {
		client, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept PostgreSQL connection: %w", err)
		}
		go p.serveConnection(ctx, client)
	}
}

func (p postgresProxy) serveConnection(parent context.Context, client net.Conn) {
	upstream, err := (&net.Dialer{}).DialContext(parent, "tcp", p.upstream)
	if err != nil {
		_ = client.Close()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	stopClose := context.AfterFunc(ctx, func() {
		_ = client.Close()
		_ = upstream.Close()
	})
	defer stopClose()
	state := newPostgresConnState()
	errs := make(chan error, 2)
	go func() { errs <- p.relayFrontend(client, upstream, state) }()
	go func() { errs <- p.relayBackend(ctx, upstream, client, state) }()
	<-errs
	cancel()
	_ = client.Close()
	_ = upstream.Close()
	<-errs
}

func (p postgresProxy) relayFrontend(
	source net.Conn,
	destination net.Conn,
	state *postgresConnState,
) error {
	startup, err := readPostgresStartup(source)
	if err != nil {
		return err
	}
	if err := writeFull(destination, startup); err != nil {
		return err
	}
	for {
		messageType, body, raw, err := readPostgresMessage(source)
		if err != nil {
			return err
		}
		query := frontendQuery(messageType, body, state)
		if query != "" {
			state.observeQuery(query)
			if isClaimQuery(query) && p.controller.tryTrip(faultPostgresClaim, "bridge_delegations/claim") {
				return discardPostgresUntilClose(source)
			}
		}
		if err := writeFull(destination, raw); err != nil {
			return err
		}
	}
}

func (p postgresProxy) relayBackend(
	ctx context.Context,
	source net.Conn,
	destination net.Conn,
	state *postgresConnState,
) error {
	for {
		messageType, _, raw, err := readPostgresMessage(source)
		if err != nil {
			return err
		}
		if messageType == 'Z' && state.takeCommitTrap() &&
			p.controller.tryTrip(faultPostgresCommit, "bridge_appservice_transactions/commit") {
			<-ctx.Done()
			return context.Cause(ctx)
		}
		if err := writeFull(destination, raw); err != nil {
			return err
		}
	}
}

func readPostgresStartup(reader io.Reader) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint32(header))
	if length < 8 || length > maxPostgresMessageBytes {
		return nil, fmt.Errorf("invalid PostgreSQL startup length %d", length)
	}
	message := make([]byte, length)
	copy(message, header)
	if _, err := io.ReadFull(reader, message[4:]); err != nil {
		return nil, err
	}
	return message, nil
}

func readPostgresMessage(reader io.Reader) (byte, []byte, []byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, nil, nil, err
	}
	length := int(binary.BigEndian.Uint32(header[1:]))
	if length < 4 || length > maxPostgresMessageBytes {
		return 0, nil, nil, fmt.Errorf("invalid PostgreSQL message length %d", length)
	}
	body := make([]byte, length-4)
	if _, err := io.ReadFull(reader, body); err != nil {
		return 0, nil, nil, err
	}
	raw := make([]byte, len(header)+len(body))
	copy(raw, header)
	copy(raw[len(header):], body)
	return header[0], body, raw, nil
}

func frontendQuery(messageType byte, body []byte, state *postgresConnState) string {
	switch messageType {
	case 'Q':
		query, _ := postgresCString(body)
		return query
	case 'P':
		statement, rest := postgresCString(body)
		query, _ := postgresCString(rest)
		state.rememberStatement(statement, query)
		return query
	case 'B':
		_, rest := postgresCString(body)
		statement, _ := postgresCString(rest)
		return state.statement(statement)
	default:
		return ""
	}
}

func postgresCString(data []byte) (string, []byte) {
	index := bytes.IndexByte(data, 0)
	if index < 0 {
		return "", nil
	}
	return string(data[:index]), data[index+1:]
}

func discardPostgresUntilClose(reader io.Reader) error {
	// Extended-query clients may have already pipelined Bind/Execute/Sync after the
	// trapped Parse frame. Drain those client-side frames without forwarding the
	// claim, then return when SIGKILL closes the bridge connection.
	for {
		if _, _, _, err := readPostgresMessage(reader); err != nil {
			return err
		}
	}
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
