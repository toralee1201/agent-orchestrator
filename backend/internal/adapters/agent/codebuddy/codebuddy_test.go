package codebuddy_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/codebuddy"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestManifestAndRegistry(t *testing.T) {
	manifest := codebuddy.New().Manifest()
	if manifest.ID != "codebuddy" || manifest.Name != "CodeBuddy" {
		t.Fatalf("manifest = %#v", manifest)
	}
	if !reflect.DeepEqual(manifest.Capabilities, []adapters.Capability{adapters.CapabilityAgent}) {
		t.Fatalf("capabilities = %#v", manifest.Capabilities)
	}
	found := false
	for _, adapter := range registry.Constructors() {
		if adapter.Manifest().ID == "codebuddy" {
			found = true
			if !adapter.(ports.AgentTerminalVersionRefresher).SupportsTerminalVersionRefresh() {
				t.Fatal("CodeBuddy does not support terminal version refresh")
			}
		}
	}
	if !found {
		t.Fatal("codebuddy is not registered")
	}
	if !domain.HarnessCodeBuddy.IsKnown() {
		t.Fatal("HarnessCodeBuddy is not a known harness")
	}
}

func TestLaunchCommandMapsFlags(t *testing.T) {
	plugin := codebuddy.NewWithBinary("codebuddy")
	prompt := filepath.Join(t.TempDir(), "standing.txt")
	if err := os.WriteFile(prompt, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, err := plugin.GetLaunchCommand(context.Background(), ports.LaunchConfig{
		Prompt:           "do the thing",
		Permissions:      ports.PermissionModeAcceptEdits,
		SystemPromptFile: prompt,
		NativeSessionID:  "sess-1",
		Config:           ports.AgentConfig{Model: "glm-5.3", Effort: "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"codebuddy",
		"--permission-mode", "acceptEdits",
		"--model", "glm-5.3",
		"--effort", "high",
		"--append-system-prompt", "from file",
		"--session-id", "sess-1",
	}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("argv = %#v, want %#v", cmd, want)
	}
	for _, banned := range []string{"--acp", "--print", "--serve", "--system-prompt", "--agent", "-y"} {
		for _, arg := range cmd {
			if arg == banned {
				t.Fatalf("argv contains %s: %#v", banned, cmd)
			}
		}
	}
}

func TestLaunchOmitsUnsetModelEffortAndSession(t *testing.T) {
	plugin := codebuddy.NewWithBinary("codebuddy")
	cmd, err := plugin.GetLaunchCommand(context.Background(), ports.LaunchConfig{
		Permissions:  ports.PermissionModeDefault,
		SystemPrompt: "be terse",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codebuddy", "--permission-mode", "default", "--append-system-prompt", "be terse"}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("argv = %#v, want %#v", cmd, want)
	}
}

func TestPermissionModes(t *testing.T) {
	plugin := codebuddy.NewWithBinary("codebuddy")
	cases := []struct {
		mode ports.PermissionMode
		want string
	}{
		{ports.PermissionModeDefault, "default"},
		{ports.PermissionModeAcceptEdits, "acceptEdits"},
		{ports.PermissionModeAuto, "auto"},
		{ports.PermissionModeBypassPermissions, "bypassPermissions"},
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
	plugin := codebuddy.NewWithBinary("codebuddy")
	cmd, ok, err := plugin.GetRestoreCommand(context.Background(), ports.RestoreConfig{
		Permissions: ports.PermissionModeBypassPermissions,
		Config:      ports.AgentConfig{Effort: "max"},
		Session: ports.SessionRef{Metadata: map[string]string{
			ports.MetadataKeyAgentSessionID: "native-7",
		}},
	})
	if err != nil || !ok {
		t.Fatalf("restore ok=%v err=%v", ok, err)
	}
	want := []string{
		"codebuddy",
		"--permission-mode", "bypassPermissions",
		"--effort", "max",
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
	cmd := codebuddy.AppendGrantedDirectories([]string{"codebuddy"}, nil)
	if len(cmd) != 1 {
		t.Fatalf("empty grant = %#v", cmd)
	}
	cmd = codebuddy.AppendGrantedDirectories(cmd, []string{"", " D:\\extra ", "E:\\outside"})
	want := []string{"codebuddy", "--add-dir", "D:\\extra", "--add-dir", "E:\\outside"}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("grant = %#v, want %#v", cmd, want)
	}
}

func TestRejectsUnknownEffort(t *testing.T) {
	plugin := codebuddy.NewWithBinary("codebuddy")
	_, err := plugin.GetLaunchCommand(context.Background(), ports.LaunchConfig{
		Config: ports.AgentConfig{Effort: "ultra"},
	})
	if err == nil {
		t.Fatal("expected unsupported effort")
	}
}

func TestPromptDelivery(t *testing.T) {
	strategy, err := codebuddy.New().GetPromptDeliveryStrategy(context.Background(), ports.LaunchConfig{})
	if err != nil || strategy != ports.PromptDeliveryAfterStart {
		t.Fatalf("strategy = %q, %v", strategy, err)
	}
	hints, err := codebuddy.New().PromptReadinessHints(context.Background(), ports.LaunchConfig{})
	if err != nil || hints.Timeout != 8*time.Second || !reflect.DeepEqual(hints.Patterns, []string{"CodeBuddy"}) {
		t.Fatalf("hints = %#v, %v", hints, err)
	}
}

func TestParseVersion(t *testing.T) {
	version, ok := codebuddy.ParseVersion("2.182.0\n")
	if !ok || version != "2.182.0" {
		t.Fatalf("version = %q, %v", version, ok)
	}
	if _, ok := codebuddy.ParseVersion("not a version"); ok {
		t.Fatal("accepted a non-version")
	}
}

func TestReadVersionFromLauncher(t *testing.T) {
	dir := t.TempDir()
	var path string
	if runtime.GOOS == "windows" {
		path = filepath.Join(dir, "codebuddy.cmd")
		body := "@echo off\r\necho 2.182.0\r\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	} else {
		path = filepath.Join(dir, "codebuddy")
		body := "#!/bin/sh\necho 2.182.0\n"
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	version, err := codebuddy.ReadVersion(context.Background(), path)
	if err != nil || version != "2.182.0" {
		t.Fatalf("version = %q, %v", version, err)
	}
}

func TestResolveBinaryInstallLocations(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if runtime.GOOS == "windows" {
		root := t.TempDir()
		t.Setenv("LOCALAPPDATA", root)
		path := filepath.Join(root, "codebuddy", "bin", "codebuddy.exe")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := codebuddy.New().ResolveBinary(context.Background())
		if err != nil || got != path {
			t.Fatalf("resolved %q, %v; want %s", got, err, path)
		}
		return
	}
	path := filepath.Join(dir, "cbc")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := codebuddy.New().ResolveBinary(context.Background())
	if err != nil || got != path {
		t.Fatalf("resolved %q, %v; want %s", got, err, path)
	}
}
