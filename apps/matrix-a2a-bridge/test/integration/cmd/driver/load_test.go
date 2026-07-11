package main

import (
	"strings"
	"testing"
)

func TestParseBridgeMetrics(t *testing.T) {
	metrics, err := parseBridgeMetrics([]byte(strings.TrimSpace(`
# HELP ignored ignored
go_memstats_heap_alloc_bytes 1.5e+06
go_memstats_heap_inuse_bytes 2e+06
go_memstats_heap_sys_bytes 4e+06
fgentic_queue_depth 90
fgentic_inflight_delegations 10
fgentic_dedup_skips_total 100
fgentic_delegations_total{ghost="agent-integration",outcome="ok"} 100
`) + "\n"))
	if err != nil {
		t.Fatalf("parseBridgeMetrics: %v", err)
	}
	if metrics.HeapAlloc != 1_500_000 || metrics.HeapInUse != 2_000_000 || metrics.HeapSys != 4_000_000 {
		t.Fatalf("heap metrics = %+v", metrics)
	}
	if metrics.QueueDepth != 90 || metrics.InFlight != 10 || metrics.DedupSkips != 100 {
		t.Fatalf("bridge metrics = %+v", metrics)
	}
}

func TestParseBridgeMetricsRequiresEverySignal(t *testing.T) {
	_, err := parseBridgeMetrics([]byte("fgentic_queue_depth 0\n"))
	if err == nil || !strings.Contains(err.Error(), "is missing") {
		t.Fatalf("parseBridgeMetrics error = %v", err)
	}
}

func TestAssertPerRoomFIFO(t *testing.T) {
	records := []stubRequestRecord{
		{Room: 0, Sequence: 0},
		{Room: 1, Sequence: 0},
		{Room: 0, Sequence: 1},
		{Room: 1, Sequence: 1},
	}
	if err := assertPerRoomFIFO("start", records, 2, 2); err != nil {
		t.Fatalf("assertPerRoomFIFO: %v", err)
	}
	records[2].Sequence = 2
	if err := assertPerRoomFIFO("start", records, 2, 2); err == nil {
		t.Fatal("assertPerRoomFIFO accepted reordered sequence")
	}
}

func TestLoadConfigResourceLimit(t *testing.T) {
	t.Setenv("LOAD_ROOMS", "20")
	t.Setenv("LOAD_MESSAGES_PER_ROOM", "11")
	if _, err := loadConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "resource-safe limit") {
		t.Fatalf("loadConfigFromEnv error = %v", err)
	}
}

func TestAssertLoadInvariants(t *testing.T) {
	const rooms = 10
	const perRoom = 10
	records := make([]stubRequestRecord, 0, rooms*perRoom)
	for sequence := range perRoom {
		for room := range rooms {
			records = append(records, stubRequestRecord{Room: room, Sequence: sequence})
		}
	}
	stats := stubStats{
		DelayMillis:    2000,
		MaxActive:      rooms,
		TotalStarted:   rooms * perRoom,
		TotalCompleted: rooms * perRoom,
		Starts:         records,
		Completions:    records,
	}
	profile := metricProfile{
		Peak:  bridgeMetrics{HeapAlloc: 8 << 20, HeapInUse: 10 << 20, QueueDepth: 90, InFlight: rooms},
		Final: bridgeMetrics{DedupSkips: rooms * perRoom},
	}
	cfg := loadConfig{
		Rooms:           rooms,
		MessagesPerRoom: perRoom,
		ConcurrencyCap:  defaultConcurrencyCap,
		MaxHeapBytes:    defaultMaxHeapBytes,
	}
	if err := assertLoadInvariants(cfg, stats, profile, rooms*perRoom); err != nil {
		t.Fatalf("assertLoadInvariants: %v", err)
	}

	stats.MaxActive = defaultConcurrencyCap + 1
	if err := assertLoadInvariants(cfg, stats, profile, rooms*perRoom); err == nil {
		t.Fatal("assertLoadInvariants accepted concurrency above the cap")
	}
}
