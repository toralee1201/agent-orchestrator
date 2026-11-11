package qoder_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/qoder"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestManifestAndRegistry(t *testing.T) {
	manifest := qoder.New().Manifest()
	if manifest.ID != "qoder" || manifest.Name != "Qoder" {
		t.Fatalf("manifest = %#v", manifest)
	}
	if !reflect.DeepEqual(manifest.Capabilities, []adapters.Capability{adapters.CapabilityAgent}) {
		t.Fatalf("capabilities = %#v", manifest.Capabilities)
	}
	found := false
	for _, adapter := range registry.Constructors() {
		if adapter.Manifest().ID == "qoder" {
			found = true
			if !adapter.(ports.AgentTerminalVersionRefresher).SupportsTerminalVersionRefresh() {
				t.Fatal("Qoder does not support terminal version refresh")
			}
		}
	}
	if !found {
		t.Fatal("qoder is not registered")
	}
	if !domain.HarnessQoder.IsKnown() {
		t.Fatal("HarnessQoder is not a known harness")
	}
}

func TestLaunchCommandMapsFlags(t *testing.T) {
	plugin := qoder.NewWithBinary("qodercli")
	prompt := filepath.Join(t.TempDir(), "standing.txt")
	if err := os.WriteFile(prompt, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, err := plugin.GetLaunchCommand(context.Background(), ports.LaunchConfig{
		Prompt:           "do the thing",
		Permissions:      ports.PermissionModeAcceptEdits,
		SystemPromptFile: prompt,
		NativeSessionID:  "sess-1",
		Config:           ports.AgentConfig{Model: "GLM-5.3", Effort: "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"qodercli",
		"--permission-mode", "accept_edits",
		"--model", "GLM-5.3",
		"--reasoning-effort", "high",
		"--append-system-prompt", "from file",
		"--session-id", "sess-1",
	}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("argv = %#v, want %#v", cmd, want)
	}
	for _, banned := range []string{"--acp", "--print", "-p", "--agent", "--dangerously-skip-permissions"} {
		for _, arg := range cmd {
			if arg == banned {
				t.Fatalf("argv contains %s: %#v", banned, cmd)
			}
		}
	}
}

func TestLaunchOmitsUnsetModelEffortAndSession(t *testing.T) {
	plugin := qoder.NewWithBinary("qodercli")
	cmd, err := plugin.GetLaunchCommand(context.Background(), ports.LaunchConfig{
		Permissions:  ports.PermissionModeDefault,
		SystemPrompt: "be terse",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"qodercli", "--permission-mode", "default", "--append-system-prompt", "be terse"}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("argv = %#v, want %#v", cmd, want)
	}
}

func TestPermissionModes(t *testing.T) {
	plugin := qoder.NewWithBinary("qodercli")
	cases := []struct {
		mode ports.PermissionMode
		want string
	}{
		{ports.PermissionModeDefault, "default"},
		{ports.PermissionModeAcceptEdits, "accept_edits"},
		{ports.PermissionModeAuto, "auto"},
		{ports.PermissionModeBypassPermissions, "bypass_permissions"},
		{"plan", "default"},
		{"dontAsk", "default"},
	}
	for _, tc := range cases {
		cmd, err := plugin.GetLaunchCommand(context.Background(), ports.LaunchConfig{Permissions: tc.mode})
		if err != nil {
			t.Fatal(err)
		}
		if cmd[2] != tc.want {
			t.Fatalf("mode %q mapped to %q, want %q", tc.mode, cmd[2], tc.want)
		}
	}
}

func TestRestoreUsesSessionIDAndResume(t *testing.T) {
	plugin := qoder.NewWithBinary("qodercli")
	cmd, ok, err := plugin.GetRestoreCommand(context.Background(), ports.RestoreConfig{
		Permissions: ports.PermissionModeBypassPermissions,
		Config:      ports.AgentConfig{Effort: "ultracode"},
		Session: ports.SessionRef{Metadata: map[string]string{
			ports.MetadataKeyAgentSessionID: "native-7",
		}},
	})
	if err != nil || !ok {
		t.Fatalf("restore ok=%v err=%v", ok, err)
	}
	want := []string{
		"qodercli",
		"--permission-mode", "bypass_permissions",
		"--reasoning-effort", "ultracode",
		"--session-id", "native-7",
		"--resume", "native-7",
	}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("argv = %#v, want %#v", cmd, want)
	}
	_, ok, err = plugin.GetRestoreCommand(context.Background(), ports.RestoreConfig{})
	if err != nil || ok {
		t.Fatalf("missing id ok=%v err=%v", ok, err)
	}
}

func TestGrantedDirectoriesAreOptIn(t *testing.T) {
	cmd := qoder.AppendGrantedDirectories([]string{"qodercli"}, nil)
	if len(cmd) != 1 {
		t.Fatalf("empty grant = %#v", cmd)
	}
	cmd = qoder.AppendGrantedDirectories(cmd, []string{"", " D:\\extra ", "E:\\outside"})
	want := []string{"qodercli", "--add-dir", "D:\\extra", "--add-dir", "E:\\outside"}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("grant = %#v, want %#v", cmd, want)
	}
}

func TestRejectsUnknownEffort(t *testing.T) {
	plugin := qoder.NewWithBinary("qodercli")
	_, err := plugin.GetLaunchCommand(context.Background(), ports.LaunchConfig{
		Config: ports.AgentConfig{Effort: "dont_ask"},
	})
	if err == nil {
		t.Fatal("expected unsupported effort")
	}
}

func TestPromptDelivery(t *testing.T) {
	strategy, err := qoder.New().GetPromptDeliveryStrategy(context.Background(), ports.LaunchConfig{})
	if err != nil || strategy != ports.PromptDeliveryAfterStart {
		t.Fatalf("strategy = %q, %v", strategy, err)
	}
	hints, err := qoder.New().PromptReadinessHints(context.Background(), ports.LaunchConfig{})
	if err != nil || hints.Timeout != 8*time.Second || !reflect.DeepEqual(hints.Patterns, []string{"Qoder"}) {
		t.Fatalf("hints = %#v, %v", hints, err)
	}
}

func TestCmdLauncherRejectsMultilineInstructions(t *testing.T) {
	plugin := qoder.NewWithBinary(`C:\npm\qoder.cmd`)
	_, err := plugin.GetLaunchCommand(context.Background(), ports.LaunchConfig{
		SystemPrompt: "line one\nline two",
	})
	if err == nil || !strings.Contains(err.Error(), "qoder.cmd") {
		t.Fatalf("error = %v, want a .cmd newline rejection", err)
	}
	cmd, err := qoder.NewWithBinary(`C:\qoder\qodercli.exe`).GetLaunchCommand(context.Background(), ports.LaunchConfig{
		SystemPrompt: "line one\nline two",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cmd, "\n"), "line one\nline two") {
		t.Fatalf("command = %#v, want the multiline prompt kept for a native executable", cmd)
	}
}

func TestParseVersion(t *testing.T) {
	version, ok := qoder.ParseVersion("1.1.85\n")
	if !ok || version != "1.1.85" {
		t.Fatalf("version = %q, %v", version, ok)
	}
	if _, ok := qoder.ParseVersion("not a version"); ok {
		t.Fatal("accepted a non-version")
	}
}

func TestReadVersionFromLauncher(t *testing.T) {
	dir := t.TempDir()
	var path string
	if runtime.GOOS == "windows" {
		path = filepath.Join(dir, "qoder.cmd")
		body := "@echo off\r\necho 1.1.85\r\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	} else {
		path = filepath.Join(dir, "qoder")
		body := "#!/bin/sh\necho 1.1.85\n"
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	version, err := qoder.ReadVersion(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if version != "1.1.85" {
		t.Fatalf("version = %q", version)
	}
}
