package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestPostgresQueryTrackingFindsPreparedClaimAndLedgerCommit(t *testing.T) {
	state := newPostgresConnState()
	parseBody := appendCString(nil, "claim_stmt")
	parseBody = appendCString(parseBody, `
		WITH candidate AS (
			SELECT job_id FROM bridge_delegations
			FOR UPDATE OF candidate_job SKIP LOCKED
		) SELECT * FROM candidate`)
	query := frontendQuery('P', parseBody, state)
	if !isClaimQuery(query) {
		t.Fatalf("prepared query was not classified as a claim: %q", query)
	}
	bindBody := appendCString(nil, "portal")
	bindBody = appendCString(bindBody, "claim_stmt")
	if got := frontendQuery('B', bindBody, state); got != query {
		t.Fatalf("bound statement query = %q, want %q", got, query)
	}

	state.observeQuery("INSERT INTO bridge_appservice_transactions (transaction_id) VALUES ($1)")
	state.observeQuery("COMMIT")
	if !state.takeCommitTrap() {
		t.Fatal("ledger transaction did not arm the commit response boundary")
	}
	if state.takeCommitTrap() {
		t.Fatal("commit response boundary was not one-shot")
	}
}

func TestReadPostgresMessageRejectsOversize(t *testing.T) {
	header := []byte{'Q', 0, 0, 0, 0}
	binary.BigEndian.PutUint32(header[1:], maxPostgresMessageBytes+1)
	if _, _, _, err := readPostgresMessage(bytes.NewReader(header)); err == nil {
		t.Fatal("readPostgresMessage accepted an oversized frame")
	}
}

func appendCString(destination []byte, value string) []byte {
	destination = append(destination, value...)
	return append(destination, 0)
}
