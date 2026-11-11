package codebuddy_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/codebuddy"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func writeCodeBuddySettings(t *testing.T, body string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".codebuddy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAuthStatusGatewayCredentialIsAuthorized(t *testing.T) {
	writeCodeBuddySettings(t, `{"gateway":{"auth":"password","password":"s3cret-value"}}`)
	plugin := codebuddy.NewWithBinary("codebuddy")
	status, err := plugin.AuthStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status != ports.AgentAuthStatusAuthorized {
		t.Fatalf("status = %q, want authorized", status)
	}
}

func TestAuthStatusWithoutCredentialStaysUnknown(t *testing.T) {
	writeCodeBuddySettings(t, `{"gateway":{"auth":"none"},"model":"glm-5.3"}`)
	plugin := codebuddy.NewWithBinary("codebuddy")
	status, err := plugin.AuthStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status != ports.AgentAuthStatusUnknown {
		t.Fatalf("status = %q, want unknown", status)
	}
}

func TestAuthStatusMissingSettingsFileStaysUnknown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	plugin := codebuddy.NewWithBinary("codebuddy")
	status, err := plugin.AuthStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status != ports.AgentAuthStatusUnknown {
		t.Fatalf("status = %q, want unknown", status)
	}
}

func TestAuthStatusUnparsableSettingsStaysUnknown(t *testing.T) {
	writeCodeBuddySettings(t, `not json at all`)
	plugin := codebuddy.NewWithBinary("codebuddy")
	status, err := plugin.AuthStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status != ports.AgentAuthStatusUnknown {
		t.Fatalf("status = %q, want unknown", status)
	}
}
