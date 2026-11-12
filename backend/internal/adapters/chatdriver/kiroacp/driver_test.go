package kiroacp

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

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/kiro"
	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
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
	if os.Getenv("KIRO_ACP_FAKE") == "1" && len(os.Args) > 1 && os.Args[1] == "acp" {
		runFakeStdio()
		return
	}
	os.Exit(m.Run())
}

// TestLaunchArgs asserts the launch argv starts with the `acp` subcommand and
// nothing else. Kiro has no cwd, system-prompt, or permission flags, and the
// v2-engine-only flags must never appear: they would force the v2 engine and
// defeat the default v3 handshake. --auth-method keeps its `cli` default.
func TestLaunchArgs(t *testing.T) {
	tests := []struct {
		name string
		cfg  acpdriver.LaunchConfig
	}{
		{name: "empty"},
		{
			name: "model effort prompt permissions and roots are ignored",
			cfg: acpdriver.LaunchConfig{
				WorkspacePath: `D:\work`, Model: "claude-sonnet-4.6", Effort: "high",
				Permissions: ports.PermissionModeBypassPermissions, SystemPrompt: "Follow AO rules.",
				AdditionalDirectories: []string{`D:\extra`},
			},
		},
		{name: "resume id is ignored", cfg: acpdriver.LaunchConfig{ProviderConversationID: "s-123"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, env, err := configure(context.Background(), tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if env != nil || !reflect.DeepEqual(got, []string{"acp"}) {
				t.Fatalf("args/env = %#v %#v, want [acp] / nil", got, env)
			}
			joined := strings.Join(got, "\n")
			for _, banned := range []string{
				"--agent", "--model", "--effort", "--trust-all-tools", "--trust-tools",
				"--agent-engine", "--auth-method", "--login", "--verbose",
				"--cwd", "--append-system-prompt", "--permission-mode", "chat", "serve",
			} {
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
		SessionID:             "kiro-fake",
		DataDir:               t.TempDir(),
		WorkspacePath:         workspace,
		Permissions:           ports.PermissionModeAcceptEdits,
		SystemPrompt:          "Follow AO rules.",
		AdditionalDirectories: []string{filepath.Join(workspace, "extra")},
		Env: map[string]string{
			"KIRO_ACP_FAKE":     "1",
			"KIRO_ACP_WIRE_LOG": logPath,
			"KIRO_ACP_ARGV":     argvPath,
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
	if len(gotArgs) != 1 || gotArgs[0] != "acp" {
		t.Fatalf("argv = %v, want exactly [acp]", gotArgs)
	}

	events := conv.Events()
	go func() {
		for range events { //nolint:revive // drain so the conversation keeps delivering
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
	// Standing instructions ride in the first prompt's text preamble: there is
	// no system-prompt flag to use instead.
	if !strings.Contains(text, "Follow AO rules.") || !strings.Contains(text, "ao-standing-instructions") {
		t.Fatalf("wire missing standing-instructions preamble:\n%s", text)
	}
	// No credentials or private keys are ever sent over the wire.
	for _, banned := range []string{
		"accessToken", "authSession", "KIRO_API_KEY", "PRIVATE KEY", "-----BEGIN",
	} {
		if strings.Contains(text, banned) {
			t.Fatalf("wire contains %q", banned)
		}
	}
	if conv.ProviderConversationID() != "kiro-session-1" {
		t.Fatalf("session id = %q", conv.ProviderConversationID())
	}
}

func TestValidateInitialize(t *testing.T) {
	if err := validateInitialize(acpsdk.InitializeResponse{
		AgentInfo: &acpsdk.Implementation{Name: agentInfoName, Version: "3.2.1"},
	}); err != nil {
		t.Fatalf("admitted server rejected: %v", err)
	}
	for _, resp := range []acpsdk.InitializeResponse{
		{},
		{AgentInfo: &acpsdk.Implementation{Name: "some-other-acp", Version: "3.2.1"}},
	} {
		if err := validateInitialize(resp); err == nil {
			t.Fatalf("admitted unexpected agent info: %+v", resp.AgentInfo)
		}
	}
}

func TestSessionPreamble(t *testing.T) {
	if got := sessionPreamble(acpdriver.LaunchConfig{}); got != nil {
		t.Fatalf("empty system prompt produced %v", got)
	}
	got := sessionPreamble(acpdriver.LaunchConfig{SystemPrompt: "  Follow AO rules.  "})
	if len(got) != 1 {
		t.Fatalf("preamble blocks = %d, want 1", len(got))
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Follow AO rules.") || !strings.Contains(string(raw), "ao-standing-instructions") {
		t.Fatalf("preamble is not a tagged text block: %s", raw)
	}
}

func TestProbeUsesResolvedBinary(t *testing.T) {
	plugin := &fakePlugin{binary: os.Args[0], status: ports.AgentAuthStatusUnknown}
	driver := newDriver(plugin, nil)
	caps, err := driver.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if driver.Harness() != domain.HarnessKiro {
		t.Fatalf("harness = %q", driver.Harness())
	}
	if !caps.Has(ports.ChatCapabilityApprovals) {
		t.Fatalf("caps = %#v", caps)
	}
	if plugin.resolveCalls != 1 {
		t.Fatalf("resolve calls = %d", plugin.resolveCalls)
	}
}

// TestInstalledKiroAutoStarts runs the real `kiro-cli acp` initialize handshake
// (and the binary resolution that finds it) without sending a prompt. It skips
// when the installed CLI is absent, so it is safe on machines without Kiro.
func TestInstalledKiroAutoStarts(t *testing.T) {
	binary, err := kiro.ResolveKiroBinary(context.Background())
	if err != nil {
		if errors.Is(err, ports.ErrAgentBinaryNotFound) {
			t.Skip("kiro-cli binary is absent")
		}
		t.Fatalf("resolve kiro-cli: %v", err)
	}
	driver := newDriver(&fakePlugin{binary: binary}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conv, err := driver.Start(ctx, ports.ChatStartConfig{
		SessionID: "kiro-real-auto", DataDir: t.TempDir(), WorkspacePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("real Kiro ACP startup: %v", err)
	}
	t.Cleanup(func() {
		if stopper, ok := conv.(interface{ Terminate() error }); ok {
			_ = stopper.Terminate()
		}
	})
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

func runFakeStdio() {
	if argv := os.Getenv("KIRO_ACP_ARGV"); argv != "" {
		_ = os.WriteFile(argv, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	}
	logPath := os.Getenv("KIRO_ACP_WIRE_LOG")
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
				"agentInfo":       map[string]any{"name": agentInfoName, "version": "3.2.1"},
				"agentCapabilities": map[string]any{
					"loadSession":         true,
					"promptCapabilities":  map[string]any{"image": true, "embeddedContext": false},
					"sessionCapabilities": map[string]any{},
				},
			}
		case "session/new", "session/load", "session/resume":
			result = map[string]any{"sessionId": "kiro-session-1"}
		case "session/prompt":
			_ = writeJSON(out, map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params": map[string]any{
					"sessionId": "kiro-session-1",
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
				"id":      msg.ID,
				"error":   map[string]any{"code": -32601, "message": "method not found"},
			})
			continue
		}
		_ = writeJSON(out, map[string]any{
			"jsonrpc": "2.0",
			"id":      msg.ID,
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
