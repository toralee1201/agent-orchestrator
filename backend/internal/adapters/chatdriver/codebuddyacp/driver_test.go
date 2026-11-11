package codebuddyacp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestMain(m *testing.M) {
	if len(os.Args) >= 7 && os.Args[1] == "chat-host" {
		protocol := persistenthost.ProtocolRaw
		fingerprint := ""
		separator := 5
		if os.Args[5] == string(persistenthost.ProtocolACP) {
			protocol = persistenthost.ProtocolACP
			if len(os.Args) > 6 {
				fingerprint = os.Args[6]
			}
			separator = 7
		}
		if len(os.Args) <= separator || os.Args[separator] != "--" {
			os.Exit(2)
		}
		err := persistenthost.Run(context.Background(), persistenthost.Config{
			SessionID: os.Args[2], DataDir: os.Args[3], Workdir: os.Args[4],
			Env: os.Environ(), Argv: os.Args[separator+1:], Protocol: protocol,
			OwnershipFingerprint: fingerprint,
		})
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv("CODEBUDDY_ACP_FAKE") == "1" && len(os.Args) > 1 && os.Args[1] == "--acp" {
		runFakeStdio()
		return
	}
	os.Exit(m.Run())
}

func TestLaunchArgs(t *testing.T) {
	tests := []struct {
		name    string
		cfg     acpdriver.LaunchConfig
		want    []string
		wantErr error
	}{
		{
			name: "acp and default permission only",
			want: []string{"--acp", "--permission-mode", "default"},
		},
		{
			name: "model effort prompt and granted roots",
			cfg: acpdriver.LaunchConfig{
				Model: "glm-5.3", Effort: "high", Permissions: ports.PermissionModeAcceptEdits,
				SystemPrompt: "Follow AO rules.", AdditionalDirectories: []string{" D:\\extra ", ""},
			},
			want: []string{
				"--acp", "--permission-mode", "acceptEdits", "--model", "glm-5.3",
				"--effort", "high", "--append-system-prompt", "Follow AO rules.",
				"--add-dir", "D:\\extra",
			},
		},
		{
			name: "auto and bypass",
			cfg:  acpdriver.LaunchConfig{Permissions: ports.PermissionModeAuto},
			want: []string{"--acp", "--permission-mode", "auto"},
		},
		{
			name: "bypass",
			cfg:  acpdriver.LaunchConfig{Permissions: ports.PermissionModeBypassPermissions},
			want: []string{"--acp", "--permission-mode", "bypassPermissions"},
		},
		{
			name: "resume",
			cfg:  acpdriver.LaunchConfig{ProviderConversationID: "s-123"},
			want: []string{"--acp", "--permission-mode", "default", "--session-id", "s-123", "--resume"},
		},
		{
			name:    "bad session id",
			cfg:     acpdriver.LaunchConfig{ProviderConversationID: "-nope"},
			wantErr: ports.ErrChatResumeFailed,
		},
		{
			name:    "unknown effort",
			cfg:     acpdriver.LaunchConfig{Effort: "ultra"},
			wantErr: ports.ErrUnsupportedEffort,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, env, err := configure(context.Background(), tt.cfg)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if env != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("args/env = %#v %#v, want %#v", got, env, tt.want)
			}
			joined := strings.Join(got, "\n")
			for _, banned := range []string{"-y", "sessionAdmissionV2", "accessToken", "authSession"} {
				if strings.Contains(joined, banned) {
					t.Fatalf("args contain %q: %v", banned, got)
				}
			}
		})
	}
}

func TestFakeStdioInitializeNewAndPrompt(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "wire.log")
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	plugin := &fakePlugin{binary: os.Args[0]}
	driver := newDriver(plugin, nil)
	workspace := t.TempDir()
	conv, err := driver.Start(context.Background(), ports.ChatStartConfig{
		SessionID:             "codebuddy-fake",
		DataDir:               t.TempDir(),
		WorkspacePath:         workspace,
		Model:                 "hy4std",
		Effort:                "low",
		Permissions:           ports.PermissionModeAcceptEdits,
		SystemPrompt:          "standing",
		AdditionalDirectories: []string{filepath.Join(workspace, "extra")},
		Env: map[string]string{
			"CODEBUDDY_ACP_FAKE":     "1",
			"CODEBUDDY_ACP_WIRE_LOG": logPath,
			"CODEBUDDY_ACP_ARGV":     argvPath,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	gotArgs := strings.Split(strings.TrimSpace(string(argv)), "\n")
	for _, want := range []string{"--acp", "--model", "hy4std", "--effort", "low", "--permission-mode", "acceptEdits", "--append-system-prompt", "standing", "--add-dir"} {
		if !contains(gotArgs, want) {
			t.Fatalf("argv %v missing %q", gotArgs, want)
		}
	}
	if contains(gotArgs, "--resume") || contains(gotArgs, "--session-id") {
		t.Fatalf("fresh start resumed: %v", gotArgs)
	}

	events := conv.Events()
	go func() {
		for range events {
		}
	}()
	ref, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "hello"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	starter, ok := conv.(interface{ StartDeferredTurn(string) error })
	if !ok {
		t.Fatal("conversation does not dispatch deferred turns")
	}
	if err := starter.StartDeferredTurn(ref.ProviderTurnID); err != nil {
		t.Fatalf("StartDeferredTurn: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var wire []byte
	for {
		wire, err = os.ReadFile(logPath)
		if err == nil && strings.Contains(string(wire), "session/prompt") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("wire missing session/prompt: %v\n%s", err, wire)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopper, ok := conv.(interface{ Terminate() error })
	if !ok {
		t.Fatal("conversation cannot terminate the host")
	}
	if err := stopper.Terminate(); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	text := string(wire)
	for _, method := range []string{"initialize", "session/new", "session/prompt"} {
		if !strings.Contains(text, method) {
			t.Fatalf("wire missing %s:\n%s", method, text)
		}
	}
	for _, banned := range []string{
		"sessionAdmissionV2", "authSession", "productConfig", "runtimeTransportCredential",
		"runtimeSessionBindingV2", "runtimeBindingToken", "runtimeAuthority", "sessionGrantV1",
		"completionDispatchGrantV1", "accessToken", "_codebuddy.ai/activateWorkbuddyOwnerRuntime",
	} {
		if strings.Contains(text, banned) {
			t.Fatalf("wire contains %q", banned)
		}
	}
	if conv.ProviderConversationID() != "cb-session-1" {
		t.Fatalf("session id = %q", conv.ProviderConversationID())
	}
}

func TestProbeUsesResolvedBinary(t *testing.T) {
	plugin := &fakePlugin{binary: os.Args[0], status: ports.AgentAuthStatusUnknown}
	driver := newDriver(plugin, nil)
	caps, err := driver.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if driver.Harness() != HarnessID {
		t.Fatalf("harness = %q", driver.Harness())
	}
	if !caps.Has(ports.ChatCapabilityStreaming) || !caps.Has(ports.ChatCapabilityApprovals) {
		t.Fatalf("caps = %#v", caps)
	}
	if plugin.resolveCalls != 1 {
		t.Fatalf("resolve calls = %d", plugin.resolveCalls)
	}
}

type fakePlugin struct {
	binary       string
	status       ports.AgentAuthStatus
	resolveCalls int
}

func (p *fakePlugin) ResolveBinary(context.Context) (string, error) {
	p.resolveCalls++
	if p.binary == "" {
		return "", ports.ErrAgentBinaryNotFound
	}
	return p.binary, nil
}

func (p *fakePlugin) AuthStatus(context.Context) (ports.AgentAuthStatus, error) {
	if p.status == "" {
		return ports.AgentAuthStatusUnknown, nil
	}
	return p.status, nil
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func runFakeStdio() {
	if argv := os.Getenv("CODEBUDDY_ACP_ARGV"); argv != "" {
		_ = os.WriteFile(argv, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	}
	logPath := os.Getenv("CODEBUDDY_ACP_WIRE_LOG")
	var logFile *os.File
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(2)
		}
		logFile = f
		defer logFile.Close()
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for in.Scan() {
		line := in.Bytes()
		if logFile != nil {
			_, _ = logFile.Write(append(append([]byte{}, line...), '\n'))
			_ = logFile.Sync()
			if strings.Contains(string(line), "session/prompt") {
				_ = logFile.Close()
				logFile = nil
			}
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(line, &msg); err != nil || len(msg.ID) == 0 || string(msg.ID) == "null" {
			continue
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": 1,
				"agentCapabilities": map[string]any{
					"loadSession": true,
					"sessionCapabilities": map[string]any{
						"resume":                map[string]any{},
						"additionalDirectories": map[string]any{},
					},
				},
			}
		case "session/new", "session/load", "session/resume":
			result = map[string]any{"sessionId": "cb-session-1"}
		case "session/prompt":
			_ = writeJSON(out, map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params": map[string]any{
					"sessionId": "cb-session-1",
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": "ok"},
					},
				},
			})
			result = map[string]any{"stopReason": "end_turn"}
		case "session/set_mode", "session/set_config_option":
			result = map[string]any{}
		default:
			_ = writeJSON(out, map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(msg.ID),
				"error":   map[string]any{"code": -32601, "message": "method not found"},
			})
			continue
		}
		_ = writeJSON(out, map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(msg.ID),
			"result":  result,
		})
	}
	time.Sleep(20 * time.Millisecond)
}

func writeJSON(out *bufio.Writer, v any) error {
	enc := json.NewEncoder(out)
	if err := enc.Encode(v); err != nil {
		return err
	}
	return out.Flush()
}
