package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFaultControllerTripsOnceAndRetainsObservations(t *testing.T) {
	controller := &faultController{}
	if err := controller.arm(faultMatrixResponse); err != nil {
		t.Fatalf("arm: %v", err)
	}
	controller.observeMatrix("/send/txn-1")
	if !controller.tryTrip(faultMatrixResponse, "/send/txn-1") {
		t.Fatal("first matching fault did not trip")
	}
	if controller.tryTrip(faultMatrixResponse, "/send/txn-1") {
		t.Fatal("one-shot fault tripped twice")
	}
	controller.observeMatrix("/send/txn-1")

	snapshot := controller.snapshot()
	if snapshot.Armed || !snapshot.Tripped || snapshot.MatchedPath != "/send/txn-1" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if len(snapshot.MatrixPaths) != 2 || snapshot.MatrixPaths[0] != snapshot.MatrixPaths[1] {
		t.Fatalf("matrix paths = %v, want one deterministic path twice", snapshot.MatrixPaths)
	}
}

func TestControlHandlerRejectsUnknownMode(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/arm/not-a-mode", nil)
	(&faultController{}).controlHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusConflict)
	}
}
