package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/util/dbutil"
)

var (
	_ Store = (*Memory)(nil)
	_ Store = (*Postgres)(nil)
)

func TestDurableLedgerMigrationContract(t *testing.T) {
	if len(UpgradeTable) != 2 {
		t.Fatalf("upgrade table length = %d, want 2", len(UpgradeTable))
	}
	recorder := &databaseRecorder{}
	db := recorder.database(t)
	t.Cleanup(func() { _ = db.Close() })
	to, compat, err := UpgradeTable[0].DangerouslyRun(t.Context(), db)
	if err != nil {
		t.Fatalf("execute migration through dbutil: %v", err)
	}
	if to != 1 || compat != 1 {
		t.Fatalf("migration version = (%d, %d), want (1, 1)", to, compat)
	}
	queries := recorder.executedQueries()
	if len(queries) != 1 {
		t.Fatalf("migration exec count = %d, want 1", len(queries))
	}
	migration := queries[0]
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS bridge_processed_events",
		"CREATE TABLE bridge_appservice_transactions",
		"body_sha256",
		"CREATE TABLE bridge_delegations",
		"UNIQUE (matrix_event_id, ghost_mxid)",
		"intake_sequence             BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE",
		"lease_generation            BIGINT NOT NULL DEFAULT 0",
		"poll_count                  INTEGER NOT NULL DEFAULT 0",
		"state IN ('pending', 'a2a_prepared', 'awaiting_task', 'reply_pending', 'delivered', 'denied', 'ambiguous', 'dead')",
		"bridge_delegations_room_fifo",
		"bridge_delegations_terminal_cleanup",
	} {
		if !strings.Contains(migration, required) {
			t.Errorf("migration does not contain %q", required)
		}
	}
	if strings.Contains(migration, "-- only: postgres") || strings.Contains(migration, "-- end only postgres") {
		t.Fatal("dbutil dialect markers leaked into executed migration")
	}
}

func TestRoomWelcomeMigrationContract(t *testing.T) {
	recorder := &databaseRecorder{}
	db := recorder.database(t)
	t.Cleanup(func() { _ = db.Close() })
	to, compat, err := UpgradeTable[1].DangerouslyRun(t.Context(), db)
	if err != nil {
		t.Fatalf("execute room-welcome migration through dbutil: %v", err)
	}
	if to != 2 || compat != 2 {
		t.Fatalf("migration version = (%d, %d), want (2, 2)", to, compat)
	}
	queries := recorder.executedQueries()
	if len(queries) != 1 || !strings.Contains(queries[0], "CREATE TABLE bridge_room_welcomes") ||
		!strings.Contains(queries[0], "room_id     TEXT PRIMARY KEY") {
		t.Fatalf("room-welcome migration = %#v", queries)
	}
}

func TestPostgresMarkRoomWelcomedUsesAtomicInsert(t *testing.T) {
	recorder := &databaseRecorder{}
	db := recorder.database(t)
	t.Cleanup(func() { _ = db.Close() })
	store := &Postgres{db: db}

	first, err := store.MarkRoomWelcomed(t.Context(), "!room:example.org")
	if err != nil || !first {
		t.Fatalf("MarkRoomWelcomed = (%v, %v), want (true, nil)", first, err)
	}
	events := recorder.recordedEvents()
	if len(events) != 1 || events[0].kind != "exec" ||
		!strings.Contains(events[0].query, "INSERT INTO bridge_room_welcomes") ||
		!strings.Contains(events[0].query, "ON CONFLICT DO NOTHING") {
		t.Fatalf("room-welcome insert events = %+v", events)
	}
	assertNamedValue(t, events[0], 1, "!room:example.org")
	recorder.mode = recordWelcomeDuplicate
	again, err := store.MarkRoomWelcomed(t.Context(), "!room:example.org")
	if err != nil || again {
		t.Fatalf("duplicate MarkRoomWelcomed = (%v, %v), want (false, nil)", again, err)
	}
}

func TestClaimSQLLocksOneFIFOHead(t *testing.T) {
	for _, required := range []string{
		"FOR UPDATE OF candidate_job SKIP LOCKED",
		"earlier.room_id = candidate_job.room_id",
		"earlier.intake_sequence < candidate_job.intake_sequence",
		"earlier.terminal_at IS NULL",
		"candidate_job.next_attempt_at <= $2",
		"candidate_job.lease_expires_at <= $2",
		"lease_generation = claimed.lease_generation + 1",
	} {
		if !strings.Contains(claimJobSQL, required) {
			t.Errorf("claim query does not contain %q", required)
		}
	}
	if strings.Contains(claimJobSQL, "attempt_count = claimed.attempt_count + 1") {
		t.Fatal("claim query counts routine lease claims as failures")
	}
}

func TestPostgresTransitionWritesContextInSameTransaction(t *testing.T) {
	recorder := &databaseRecorder{}
	db := recorder.database(t)
	t.Cleanup(func() { _ = db.Close() })
	store := &Postgres{db: db}
	contextID := "ctx-accepted"
	request := TransitionRequest{
		Lease: LeaseToken{JobID: "job", Owner: "worker", Generation: 7},
		From:  StateA2APrepared,
		To:    StateAwaitingTask,
		At:    ledgerEpoch,
		Patch: TransitionPatch{A2AContextID: &contextID},
	}
	if err := store.Transition(t.Context(), request); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	events := recorder.recordedEvents()
	if len(events) != 4 || events[0].kind != "begin" || events[1].kind != "exec" ||
		events[2].kind != "exec" || events[3].kind != "commit" {
		t.Fatalf("transition database events = %+v, want begin/update/context/commit", events)
	}
	if !strings.Contains(events[1].query, "UPDATE bridge_delegations") ||
		!strings.Contains(events[1].query, "attempt_count = 0") ||
		!strings.Contains(events[1].query, "poll_count = 0") ||
		!strings.Contains(events[2].query, "INSERT INTO bridge_contexts") ||
		!strings.Contains(events[2].query, "SELECT room_id, ghost_localpart") {
		t.Fatalf("transition queries do not atomically update job and localpart context: %+v", events)
	}
	if generation, ok := events[1].valueNamed("worker", 7); !ok || generation.Value != int64(7) {
		t.Fatalf("lease generation argument = %#v, want signed bigint 7", generation.Value)
	}
}

func TestPostgresTransitionRollsBackWhenContextWriteFails(t *testing.T) {
	recorder := &databaseRecorder{mode: recordContextFailure}
	db := recorder.database(t)
	t.Cleanup(func() { _ = db.Close() })
	store := &Postgres{db: db}
	contextID := "ctx-accepted"
	err := store.Transition(t.Context(), TransitionRequest{
		Lease: LeaseToken{JobID: "job", Owner: "worker", Generation: 7},
		From:  StateA2APrepared,
		To:    StateAwaitingTask,
		At:    ledgerEpoch,
		Patch: TransitionPatch{A2AContextID: &contextID},
	})
	if err == nil || !strings.Contains(err.Error(), "store transitioned A2A context") {
		t.Fatalf("Transition context error = %v", err)
	}
	events := recorder.recordedEvents()
	if len(events) != 4 || events[0].kind != "begin" || events[1].kind != "exec" ||
		events[2].kind != "exec" || events[3].kind != "rollback" {
		t.Fatalf("failed context database events = %+v, want begin/update/context/rollback", events)
	}
}

func TestPostgresAdmissionConflictsRollBackAtomically(t *testing.T) {
	tests := []struct {
		name    string
		mode    recordingMode
		wantErr error
	}{
		{name: "transaction hash", mode: recordTransactionHashConflict, wantErr: ErrTransactionHashConflict},
		{name: "event ghost", mode: recordDelegationConflict, wantErr: ErrDelegationConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &databaseRecorder{mode: test.mode}
			db := recorder.database(t)
			t.Cleanup(func() { _ = db.Close() })
			store := &Postgres{db: db}
			_, err := store.AdmitTransaction(
				t.Context(),
				testAdmission("txn-conflict", ledgerEpoch, testDelegation("$event", "agent", "!room")),
			)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("AdmitTransaction error = %v, want %v", err, test.wantErr)
			}
			events := recorder.recordedEvents()
			if len(events) < 4 || events[0].kind != "begin" || events[len(events)-1].kind != "rollback" {
				t.Fatalf("conflicting admission database events = %+v, want transaction rollback", events)
			}
			for _, event := range events {
				if event.kind == "commit" {
					t.Fatalf("conflicting admission committed: %+v", events)
				}
			}
		})
	}
}

func TestPostgresAdmissionHonorsLegacyProcessedEventTombstone(t *testing.T) {
	recorder := &databaseRecorder{mode: recordLegacyTombstone}
	db := recorder.database(t)
	t.Cleanup(func() { _ = db.Close() })
	store := &Postgres{db: db}
	delegation := testDelegation("$legacy", "agent", "!room")
	result, err := store.AdmitTransaction(
		t.Context(),
		testAdmission("txn-legacy", ledgerEpoch, delegation),
	)
	if err != nil {
		t.Fatalf("AdmitTransaction: %v", err)
	}
	if len(result.LegacyTombstonedJobIDs) != 1 || len(result.InsertedJobIDs) != 0 || len(result.ExistingJobIDs) != 0 {
		t.Fatalf("legacy-tombstoned Postgres admission = %+v", result)
	}
	events := recorder.recordedEvents()
	if len(events) < 5 || events[0].kind != "begin" || events[len(events)-1].kind != "commit" {
		t.Fatalf("legacy admission database events = %+v, want committed transaction", events)
	}
	var guardedInsert bool
	for _, event := range events {
		if event.kind == "query" && strings.Contains(event.query, "INSERT INTO bridge_delegations") &&
			strings.Contains(event.query, "FROM bridge_processed_events") {
			guardedInsert = true
		}
	}
	if !guardedInsert {
		t.Fatal("delegation insert was not guarded by the legacy processed-event tombstone")
	}
}

func TestPostgresAdmissionSerializesCapacityAndPersistsContentFreeDenial(t *testing.T) {
	tests := []struct {
		name           string
		capacityRows   [][]driver.Value
		roomCapacity   int
		globalCapacity int
		roomID         string
		wantReason     string
	}{
		{
			name:           "room takes precedence when both limits are full",
			capacityRows:   [][]driver.Value{{"!room-full", int64(1)}, {"!other", int64(1)}},
			roomCapacity:   1,
			globalCapacity: 2,
			roomID:         "!room-full",
			wantReason:     QueueRoomCapacityRejected,
		},
		{
			name:           "global limit",
			capacityRows:   [][]driver.Value{{"!other", int64(1)}},
			roomCapacity:   2,
			globalCapacity: 1,
			roomID:         "!available-room",
			wantReason:     QueueGlobalCapacityRejected,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &databaseRecorder{capacityRows: test.capacityRows}
			db := recorder.database(t)
			t.Cleanup(func() { _ = db.Close() })
			store := &Postgres{db: db}
			delegation := testDelegation("$capacity", "agent", test.roomID)
			admission := testAdmission("txn-capacity", ledgerEpoch, delegation)
			admission.RoomCapacity = test.roomCapacity
			admission.GlobalCapacity = test.globalCapacity

			result, err := store.AdmitTransaction(t.Context(), admission)
			if err != nil {
				t.Fatalf("AdmitTransaction: %v", err)
			}
			wantJobID := JobIDFor(delegation.MatrixEventID, delegation.GhostMXID)
			if result.Disposition != TransactionAccepted || len(result.InsertedJobIDs) != 0 ||
				len(result.ExistingJobIDs) != 0 || len(result.LegacyTombstonedJobIDs) != 0 ||
				!capacityDenialsEqual(
					result.CapacityDenied,
					[]CapacityDenial{{JobID: wantJobID, Reason: test.wantReason}},
				) {
				t.Fatalf("capacity-limited Postgres admission = %+v", result)
			}

			events := recorder.recordedEvents()
			if len(events) < 6 || events[0].kind != "begin" || events[len(events)-1].kind != "commit" {
				t.Fatalf("capacity admission database events = %+v", events)
			}
			lockIndex := databaseEventIndex(events, "exec", "pg_advisory_xact_lock")
			transactionIndex := databaseEventIndex(events, "exec", "INSERT INTO bridge_appservice_transactions")
			countIndex := databaseEventIndex(events, "query", "WHERE terminal_at IS NULL")
			delegationIndex := databaseEventIndex(events, "query", "INSERT INTO bridge_delegations")
			if lockIndex < 0 || transactionIndex <= lockIndex || countIndex <= transactionIndex ||
				delegationIndex <= countIndex {
				t.Fatalf("capacity lock/count/insert order = %+v", events)
			}
			insert := events[delegationIndex]
			if !strings.Contains(insert.query, "state, next_attempt_at, error_code") ||
				!strings.Contains(insert.query, "created_at, updated_at, terminal_at") {
				t.Fatalf("capacity-denied insert lacks terminal evidence: %s", insert.query)
			}
			assertNamedValue(t, insert, 13, "")
			assertNamedValue(t, insert, 14, []byte{})
			assertNamedValue(t, insert, 15, string(StateDenied))
			assertNamedValue(t, insert, 17, test.wantReason)
			assertNamedValue(t, insert, 18, true)
			assertNamedValue(t, insert, 19, false)
			assertNamedValue(t, insert, 20, test.wantReason)
			assertNamedValue(t, insert, 27, admission.CommittedAt)
		})
	}
}

func TestPostgresCapacitySQLCountsLeasedAndDelayedJobs(t *testing.T) {
	capacitySQL := admissionCapacityLockSQL + admissionCapacityCountSQL
	for _, required := range []string{
		"pg_advisory_xact_lock",
		"WHERE terminal_at IS NULL",
		"GROUP BY room_id",
	} {
		if !strings.Contains(capacitySQL, required) {
			t.Errorf("capacity SQL does not contain %q", required)
		}
	}
	for _, forbidden := range []string{"lease_owner", "lease_expires_at", "next_attempt_at"} {
		if strings.Contains(admissionCapacityCountSQL, forbidden) {
			t.Errorf("capacity SQL incorrectly excludes %s jobs", forbidden)
		}
	}
}

func TestPostgresNonTerminalCountUsesTerminalBoundary(t *testing.T) {
	recorder := &databaseRecorder{nonTerminalCount: 7}
	db := recorder.database(t)
	t.Cleanup(func() { _ = db.Close() })
	store := &Postgres{db: db}
	got, err := store.NonTerminalCount(t.Context())
	if err != nil || got != 7 {
		t.Fatalf("NonTerminalCount = (%d, %v), want (7, nil)", got, err)
	}
	events := recorder.recordedEvents()
	if len(events) != 1 || events[0].kind != "query" ||
		!strings.Contains(events[0].query, "WHERE terminal_at IS NULL") {
		t.Fatalf("non-terminal count query = %+v", events)
	}
	for _, forbidden := range []string{"lease_owner", "lease_expires_at", "next_attempt_at"} {
		if strings.Contains(events[0].query, forbidden) {
			t.Errorf("non-terminal count query incorrectly excludes %s jobs", forbidden)
		}
	}
}

func TestPostgresAdmissionAppliesCapacityWithinAtomicBatch(t *testing.T) {
	recorder := &databaseRecorder{}
	db := recorder.database(t)
	t.Cleanup(func() { _ = db.Close() })
	store := &Postgres{db: db}
	first := testDelegation("$first", "agent", "!room")
	roomDenied := testDelegation("$second", "agent", "!room")
	otherRoom := testDelegation("$third", "agent", "!other")
	admission := testAdmission("txn-capacity-batch", ledgerEpoch, first, roomDenied, otherRoom)
	admission.RoomCapacity = 1
	admission.GlobalCapacity = 2

	result, err := store.AdmitTransaction(t.Context(), admission)
	if err != nil {
		t.Fatalf("AdmitTransaction: %v", err)
	}
	wantInserted := []string{
		JobIDFor(first.MatrixEventID, first.GhostMXID),
		JobIDFor(otherRoom.MatrixEventID, otherRoom.GhostMXID),
	}
	wantDenied := []CapacityDenial{{
		JobID:  JobIDFor(roomDenied.MatrixEventID, roomDenied.GhostMXID),
		Reason: QueueRoomCapacityRejected,
	}}
	if !stringSlicesEqual(result.InsertedJobIDs, wantInserted) ||
		!capacityDenialsEqual(result.CapacityDenied, wantDenied) {
		t.Fatalf("batch capacity result = %+v, want inserted=%v denied=%v", result, wantInserted, wantDenied)
	}
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestPostgresTransitionWithoutContextAvoidsTransaction(t *testing.T) {
	recorder := &databaseRecorder{}
	db := recorder.database(t)
	t.Cleanup(func() { _ = db.Close() })
	store := &Postgres{db: db}
	if err := store.Transition(t.Context(), TransitionRequest{
		Lease: LeaseToken{JobID: "job", Owner: "worker", Generation: 1},
		From:  StatePending,
		To:    StateA2APrepared,
		At:    ledgerEpoch,
	}); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	events := recorder.recordedEvents()
	if len(events) != 1 || events[0].kind != "exec" {
		t.Fatalf("context-free transition database events = %+v, want one exec", events)
	}
}

func TestNewPostgresRejectsNonPostgres(t *testing.T) {
	_, err := NewPostgres(t.Context(), &dbutil.Database{Dialect: dbutil.SQLite})
	if err == nil || !strings.Contains(err.Error(), "requires Postgres") {
		t.Fatalf("NewPostgres(SQLite) error = %v", err)
	}
}

type databaseEvent struct {
	kind  string
	query string
	args  []driver.NamedValue
}

func databaseEventIndex(events []databaseEvent, kind, queryFragment string) int {
	for index, event := range events {
		if event.kind == kind && strings.Contains(event.query, queryFragment) {
			return index
		}
	}
	return -1
}

func assertNamedValue(t *testing.T, event databaseEvent, ordinal int, want any) {
	t.Helper()
	for _, value := range event.args {
		if value.Ordinal != ordinal {
			continue
		}
		switch typedWant := want.(type) {
		case []byte:
			got, ok := value.Value.([]byte)
			if !ok || string(got) != string(typedWant) {
				t.Fatalf("query argument %d = %#v, want %#v", ordinal, value.Value, want)
			}
		default:
			if value.Value != want {
				t.Fatalf("query argument %d = %#v, want %#v", ordinal, value.Value, want)
			}
		}
		return
	}
	t.Fatalf("query has no argument %d: %+v", ordinal, event.args)
}

func (e databaseEvent) valueNamed(stringValue string, intValue int64) (driver.NamedValue, bool) {
	for _, value := range e.args {
		if value.Value == stringValue {
			continue
		}
		if value.Value == intValue {
			return value, true
		}
	}
	return driver.NamedValue{}, false
}

type databaseRecorder struct {
	mu               sync.Mutex
	events           []databaseEvent
	mode             recordingMode
	capacityRows     [][]driver.Value
	nonTerminalCount int64
}

type recordingMode uint8

const (
	recordSuccess recordingMode = iota
	recordContextFailure
	recordTransactionHashConflict
	recordDelegationConflict
	recordLegacyTombstone
	recordWelcomeDuplicate
)

func (r *databaseRecorder) database(t *testing.T) *dbutil.Database {
	t.Helper()
	raw := sql.OpenDB(recordingConnector{recorder: r})
	raw.SetMaxOpenConns(1)
	db, err := dbutil.NewWithDB(raw, "postgres")
	if err != nil {
		t.Fatalf("wrap recording database: %v", err)
	}
	return db
}

func (r *databaseRecorder) record(event databaseEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	event.args = append([]driver.NamedValue(nil), event.args...)
	r.events = append(r.events, event)
}

func (r *databaseRecorder) recordedEvents() []databaseEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]databaseEvent(nil), r.events...)
}

func (r *databaseRecorder) executedQueries() []string {
	events := r.recordedEvents()
	queries := make([]string, 0, len(events))
	for _, event := range events {
		if event.kind == "exec" {
			queries = append(queries, event.query)
		}
	}
	return queries
}

type recordingConnector struct {
	recorder *databaseRecorder
}

func (c recordingConnector) Connect(context.Context) (driver.Conn, error) {
	return &recordingConn{recorder: c.recorder}, nil
}

func (c recordingConnector) Driver() driver.Driver {
	return c
}

func (c recordingConnector) Open(string) (driver.Conn, error) {
	return c.Connect(context.Background())
}

type recordingConn struct {
	recorder *databaseRecorder
}

func (c *recordingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("recording driver does not prepare statements")
}

func (c *recordingConn) Close() error { return nil }

func (c *recordingConn) Begin() (driver.Tx, error) {
	c.recorder.record(databaseEvent{kind: "begin"})
	return recordingTx{recorder: c.recorder}, nil
}

func (c *recordingConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return c.Begin()
}

func (c *recordingConn) ExecContext(
	_ context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Result, error) {
	c.recorder.record(databaseEvent{kind: "exec", query: query, args: args})
	if c.recorder.mode == recordContextFailure && strings.Contains(query, "INSERT INTO bridge_contexts") {
		return nil, errors.New("forced context write failure")
	}
	if c.recorder.mode == recordTransactionHashConflict &&
		strings.Contains(query, "INSERT INTO bridge_appservice_transactions") {
		return driver.RowsAffected(0), nil
	}
	if c.recorder.mode == recordWelcomeDuplicate && strings.Contains(query, "INSERT INTO bridge_room_welcomes") {
		return driver.RowsAffected(0), nil
	}
	return driver.RowsAffected(1), nil
}

func (c *recordingConn) QueryContext(
	_ context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	c.recorder.record(databaseEvent{kind: "query", query: query, args: args})
	switch {
	case strings.Contains(query, "SELECT COUNT(*)"):
		return &recordingRows{
			columns: []string{"count"},
			values:  [][]driver.Value{{c.recorder.nonTerminalCount}},
		}, nil
	case strings.Contains(query, "SELECT room_id, COUNT(*)"):
		return &recordingRows{
			columns: []string{"room_id", "count"},
			values:  append([][]driver.Value(nil), c.recorder.capacityRows...),
		}, nil
	case c.recorder.mode == recordTransactionHashConflict && strings.Contains(query, "SELECT body_sha256"):
		return &recordingRows{
			columns: []string{"body_sha256"},
			values:  [][]driver.Value{{make([]byte, 32)}},
		}, nil
	case c.recorder.mode == recordDelegationConflict && strings.Contains(query, "INSERT INTO bridge_delegations"):
		return &recordingRows{columns: []string{"job_id"}}, nil
	case c.recorder.mode == recordDelegationConflict && strings.Contains(query, "SELECT job_id, intake_fingerprint"):
		return &recordingRows{
			columns: []string{"job_id", "intake_fingerprint"},
			values:  [][]driver.Value{{"existing-job", make([]byte, 32)}},
		}, nil
	case c.recorder.mode == recordLegacyTombstone && strings.Contains(query, "INSERT INTO bridge_delegations"):
		return &recordingRows{columns: []string{"job_id"}}, nil
	case c.recorder.mode == recordLegacyTombstone && strings.Contains(query, "SELECT job_id, intake_fingerprint"):
		return &recordingRows{columns: []string{"job_id", "intake_fingerprint"}}, nil
	case c.recorder.mode == recordLegacyTombstone && strings.Contains(query, "SELECT EXISTS"):
		return &recordingRows{
			columns: []string{"exists"},
			values:  [][]driver.Value{{true}},
		}, nil
	case strings.Contains(query, "INSERT INTO bridge_delegations"):
		var jobID driver.Value
		for _, value := range args {
			if value.Ordinal == 1 {
				jobID = value.Value
				break
			}
		}
		return &recordingRows{
			columns: []string{"job_id"},
			values:  [][]driver.Value{{jobID}},
		}, nil
	}
	return emptyRows{}, nil
}

type recordingTx struct {
	recorder *databaseRecorder
}

func (tx recordingTx) Commit() error {
	tx.recorder.record(databaseEvent{kind: "commit"})
	return nil
}

func (tx recordingTx) Rollback() error {
	tx.recorder.record(databaseEvent{kind: "rollback"})
	return nil
}

type emptyRows struct{}

func (emptyRows) Columns() []string         { return nil }
func (emptyRows) Close() error              { return nil }
func (emptyRows) Next([]driver.Value) error { return io.EOF }

type recordingRows struct {
	columns []string
	values  [][]driver.Value
	next    int
}

func (r *recordingRows) Columns() []string { return r.columns }
func (r *recordingRows) Close() error      { return nil }

func (r *recordingRows) Next(destination []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(destination, r.values[r.next])
	r.next++
	return nil
}

var (
	_ driver.Connector      = recordingConnector{}
	_ driver.ExecerContext  = (*recordingConn)(nil)
	_ driver.QueryerContext = (*recordingConn)(nil)
	_ driver.ConnBeginTx    = (*recordingConn)(nil)
)

// Keep the standard time conversion exercised by database/sql's default converter.
var _ driver.Value = time.Time{}
