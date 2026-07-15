package state

import "time"

// admissionCapacity tracks the durable non-terminal backlog while one appservice transaction is
// planned. Existing leased and delayed jobs are included by the caller; terminal tombstones are not.
type admissionCapacity struct {
	roomLimit   int64
	globalLimit int64
	globalCount int64
	roomCounts  map[string]int64
}

func newAdmissionCapacity(roomLimit, globalLimit int) admissionCapacity {
	return admissionCapacity{
		roomLimit:   int64(roomLimit),
		globalLimit: int64(globalLimit),
		roomCounts:  make(map[string]int64),
	}
}

func (capacity *admissionCapacity) add(roomID string, count int64) {
	capacity.globalCount += count
	capacity.roomCounts[roomID] += count
}

// denialReason preserves the legacy queue boundary order: a full room is reported before the
// process-wide backlog, even when both limits are exhausted.
func (capacity admissionCapacity) denialReason(roomID string) string {
	if capacity.roomCounts[roomID] >= capacity.roomLimit {
		return QueueRoomCapacityRejected
	}
	if capacity.globalCount >= capacity.globalLimit {
		return QueueGlobalCapacityRejected
	}
	return ""
}

func denyJobForCapacity(job *Job, reason string, at time.Time) {
	job.State = StateDenied
	job.ErrorCode = reason
	job.AdmissionChecked = true
	job.AdmissionAllowed = false
	job.AdmissionReason = reason
	job.UpdatedAt = at
	job.TerminalAt = at
	clearJobContent(job)
	clearLease(job)
}
