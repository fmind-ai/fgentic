package matrixapp

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"maunium.net/go/mautrix/appservice"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/config"
)

func TestConfigureMautrixLogger(t *testing.T) {
	var output bytes.Buffer
	as := appservice.Create()
	configureMautrixLogger(as, &output)

	as.Log.Debug().Str("component", "matrix").Msg("transaction received")
	if output.Len() != 0 {
		t.Fatalf("content-bearing debug log was enabled: %s", output.String())
	}
	as.Log.Info().Str("component", "matrix").Msg("appservice ready")

	var entry map[string]any
	if err := json.NewDecoder(&output).Decode(&entry); err != nil {
		t.Fatalf("decode mautrix log: %v", err)
	}
	for field, want := range map[string]string{
		"level":     "info",
		"component": "matrix",
		"message":   "appservice ready",
	} {
		if got := entry[field]; got != want {
			t.Errorf("log field %q = %v, want %q", field, got, want)
		}
	}
	if _, ok := entry["time"]; !ok {
		t.Error("mautrix log has no timestamp")
	}
}

func TestGenerateRegistrationMatchesGolden(t *testing.T) {
	path := t.TempDir() + "/registration.yaml"
	cfg := config.Config{
		RegistrationPath: path,
		ServerName:       "matrix.example",
		GhostPrefix:      "agent-",
		ListenPort:       29331,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := GenerateRegistration(cfg, log); err != nil {
		t.Fatalf("GenerateRegistration: %v", err)
	}

	got, err := appservice.LoadRegistration(path)
	if err != nil {
		t.Fatalf("load generated registration: %v", err)
	}
	if len(got.AppToken) < 32 || len(got.ServerToken) < 32 || got.AppToken == got.ServerToken {
		t.Fatal("generated registration tokens are missing or not independent")
	}
	want, err := appservice.LoadRegistration("testdata/registration.golden.yaml")
	if err != nil {
		t.Fatalf("load golden registration: %v", err)
	}
	got.AppToken = want.AppToken
	got.ServerToken = want.ServerToken
	if !reflect.DeepEqual(got, want) {
		gotYAML, _ := got.YAML()
		wantYAML, _ := want.YAML()
		t.Fatalf("generated registration differs from golden\ngot:\n%s\nwant:\n%s", gotYAML, wantYAML)
	}
}

func TestGenerateRegistrationWrapsSaveError(t *testing.T) {
	cfg := config.Config{
		RegistrationPath: t.TempDir(),
		ServerName:       "matrix.example",
		GhostPrefix:      "agent-",
		ListenPort:       29331,
	}
	err := GenerateRegistration(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "save registration") {
		t.Fatalf("GenerateRegistration error = %v, want wrapped save error", err)
	}
}

func TestNewFailsFastOnMissingRegistration(t *testing.T) {
	cfg := config.Config{
		RegistrationPath: t.TempDir() + "/missing.yaml",
		ServerName:       "matrix.example",
		HomeserverURL:    "http://matrix.example",
		ListenHost:       "127.0.0.1",
		ListenPort:       29331,
	}
	_, err := New(cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "load registration") {
		t.Fatalf("New error = %v, want wrapped registration error", err)
	}
}
