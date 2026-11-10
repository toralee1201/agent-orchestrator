package qoderacp

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
	"sync"
	"testing"
	"time"

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
	if os.Getenv("QODER_ACP_FAKE") == "1" && len(os.Args) > 1 && os.Args[1] == "--acp" {
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
			name: "cwd and default permission, no model or effort",
			cfg:  acpdriver.LaunchConfig{WorkspacePath: `D:\work`},
			want: []string{"--acp", "--cwd", `D:\work`, "--permission-mode", "default"},
		},
		{
			name: "model effort prompt and granted roots",
			cfg: acpdriver.LaunchConfig{
				WorkspacePath: `D:\work`, Model: "qfmodel", Effort: "high",
				Permissions: ports.PermissionModeAcceptEdits, SystemPrompt: "Follow AO rules.",
				AdditionalDirectories: []string{` D:\extra `, ""},
			},
			want: []string{
				"--acp", "--cwd", `D:\work`, "--permission-mode", "accept_edits",
				"--model", "qfmodel", "--reasoning-effort", "high",
				"--append-system-prompt", "Follow AO rules.", "--add-dir", `D:\extra`,
			},
		},
		{
			name: "auto and bypass use qoder cli names",
			cfg:  acpdriver.LaunchConfig{WorkspacePath: `D:\work`, Permissions: ports.PermissionModeAuto},
			want: []string{"--acp", "--cwd", `D:\work`, "--permission-mode", "auto"},
		},
		{
			name: "bypass",
			cfg:  acpdriver.LaunchConfig{WorkspacePath: `D:\work`, Permissions: ports.PermissionModeBypassPermissions},
			want: []string{"--acp", "--cwd", `D:\work`, "--permission-mode", "bypass_permissions"},
		},
		{
			name: "resume",
			cfg:  acpdriver.LaunchConfig{WorkspacePath: `D:\work`, ProviderConversationID: "s-123"},
			want: []string{"--acp", "--cwd", `D:\work`, "--permission-mode", "default", "--session-id", "s-123", "--resume"},
		},
		{
			name:    "bad session id",
			cfg:     acpdriver.LaunchConfig{WorkspacePath: `D:\work`, ProviderConversationID: "-nope"},
			wantErr: ports.ErrChatResumeFailed,
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
			for _, banned := range []string{"--system-prompt", "--settings", "-p", "--print"} {
				if contains(got, banned) {
					t.Fatalf("args contain %q: %v", banned, got)
				}
			}
		})
	}
	if sessionMode(ports.PermissionModeBypassPermissions) != "yolo" {
		t.Fatalf("ACP bypass mode = %q", sessionMode(ports.PermissionModeBypassPermissions))
	}
	if sessionMode(ports.PermissionModeAcceptEdits) != "acceptEdits" {
		t.Fatalf("ACP accept edits = %q", sessionMode(ports.PermissionModeAcceptEdits))
	}
}

func TestFakeProcessStreamsFollowUpAndSteer(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "wire.log")
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	pidPath := filepath.Join(t.TempDir(), "pid.txt")
	plugin := &fakePlugin{binary: os.Args[0]}
	driver := newDriver(plugin, nil)
	workspace := t.TempDir()
	extra := filepath.Join(workspace, "extra")
	conv, err := driver.Start(context.Background(), ports.ChatStartConfig{
		SessionID:             "qoder-fake",
		DataDir:               t.TempDir(),
		WorkspacePath:         workspace,
		Model:                 "qfmodel",
		Effort:                "high",
		Permissions:           ports.PermissionModeAcceptEdits,
		SystemPrompt:          "standing",
		AdditionalDirectories: []string{extra},
		Env: map[string]string{
			"QODER_ACP_FAKE":     "1",
			"QODER_ACP_WIRE_LOG": logPath,
			"QODER_ACP_ARGV":     argvPath,
			"QODER_ACP_PID":      pidPath,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if stopper, ok := conv.(interface{ Terminate() error }); ok {
			_ = stopper.Terminate()
		}
	})

	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	gotArgs := strings.Split(strings.TrimSpace(string(argv)), "\n")
	for _, want := range []string{"--acp", "--cwd", workspace, "--model", "qfmodel", "--reasoning-effort", "high", "--permission-mode", "accept_edits", "--append-system-prompt", "standing", "--add-dir", extra} {
		if !contains(gotArgs, want) {
			t.Fatalf("argv %v missing %q", gotArgs, want)
		}
	}
	if contains(gotArgs, "--resume") || contains(gotArgs, "--session-id") || contains(gotArgs, "--system-prompt") || contains(gotArgs, "--print") {
		t.Fatalf("fresh start argv = %v", gotArgs)
	}

	var (
		eventMu sync.Mutex
		events  []ports.ChatEvent
	)
	go func() {
		for event := range conv.Events() {
			eventMu.Lock()
			events = append(events, event)
			eventMu.Unlock()
		}
	}()

	ref, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "hello"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	if err := conv.(ports.ChatDeferredTurnStarter).StartDeferredTurn(ref.ProviderTurnID); err != nil {
		t.Fatalf("StartDeferredTurn: %v", err)
	}

	deadline := time.Now().Add(8 * time.Second)
	var sawText, sawTool, sawApproval bool
	var approval ports.ChatEvent
	for !(sawText && sawTool && sawApproval) {
		if time.Now().After(deadline) {
			t.Fatalf("events before prompt finished: text=%v tool=%v approval=%v", sawText, sawTool, sawApproval)
		}
		eventMu.Lock()
		snapshot := append([]ports.ChatEvent(nil), events...)
		eventMu.Unlock()
		for _, event := range snapshot {
			switch event.Kind {
			case ports.ChatEventMessageDelta:
				if strings.Contains(event.Delta, "live-text") {
					sawText = true
				}
			case ports.ChatEventActivityStarted:
				sawTool = true
			case ports.ChatEventApprovalRequested:
				sawApproval = true
				approval = event
			case ports.ChatEventTurnCompleted:
				t.Fatal("turn completed before streamed text, tool, and permission")
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !wireContains(t, logPath, "prompt-open") || wireContains(t, logPath, "prompt-result") {
		t.Fatal("permission was observed only after the prompt result")
	}

	steerer, ok := conv.(ports.ChatSteerer)
	if !ok {
		t.Fatal("conversation cannot steer the running turn")
	}
	steered, err := steerer.Steer(context.Background(), ref.ProviderTurnID, ports.ChatUserMessage{Text: "focus the API"})
	if err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if steered.ProviderTurnID != ref.ProviderTurnID {
		t.Fatalf("steer turn = %q, want %q", steered.ProviderTurnID, ref.ProviderTurnID)
	}
	waitWire(t, logPath, "focus the API")
	if wireContains(t, logPath, "prompt-result") {
		t.Fatal("steer waited until the process finished the turn")
	}

	if err := conv.ResolveRequest(context.Background(), approval.RequestID, ports.ChatDecision{ID: "allow"}); err != nil {
		t.Fatalf("ResolveRequest: %v", err)
	}
	waitWire(t, logPath, "prompt-result")
	var completed ports.ChatEvent
	deadline = time.Now().Add(8 * time.Second)
	for completed.Kind != ports.ChatEventTurnCompleted {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the first turn to complete")
		}
		eventMu.Lock()
		snapshot := append([]ports.ChatEvent(nil), events...)
		eventMu.Unlock()
		for _, event := range snapshot {
			if event.Kind == ports.ChatEventTurnCompleted && event.ProviderTurnID == ref.ProviderTurnID {
				completed = event
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := conv.(ports.ChatProviderEventAcknowledger).AcknowledgeProviderEvent(context.Background(), completed.ProviderEventID); err != nil {
		t.Fatalf("AcknowledgeProviderEvent: %v", err)
	}

	follow, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "second"})
	if err != nil {
		t.Fatalf("follow-up SendTurn: %v", err)
	}
	if err := conv.(ports.ChatDeferredTurnStarter).StartDeferredTurn(follow.ProviderTurnID); err != nil {
		t.Fatalf("follow-up start: %v", err)
	}
	waitWire(t, logPath, "prompt-2")

	pid, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(pid))
	if len(lines) != 1 {
		t.Fatalf("qoder processes = %q, want one", strings.TrimSpace(string(pid)))
	}
	if conv.ProviderConversationID() != "qoder-session-1" {
		t.Fatalf("session id = %q", conv.ProviderConversationID())
	}
	if driver.Harness() != domain.HarnessQoder {
		t.Fatalf("harness = %q", driver.Harness())
	}
}

func TestFakeReconnectUsesSessionID(t *testing.T) {
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	plugin := &fakePlugin{binary: os.Args[0]}
	driver := newDriver(plugin, nil)
	workspace := t.TempDir()
	conv, err := driver.Resume(context.Background(), ports.ChatResumeConfig{
		SessionID:              "qoder-fake",
		DataDir:                t.TempDir(),
		WorkspacePath:          workspace,
		ProviderConversationID: "qoder-session-1",
		Env: map[string]string{
			"QODER_ACP_FAKE": "1",
			"QODER_ACP_ARGV": argvPath,
		},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	t.Cleanup(func() {
		if stopper, ok := conv.(interface{ Terminate() error }); ok {
			_ = stopper.Terminate()
		}
	})
	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	gotArgs := strings.Split(strings.TrimSpace(string(argv)), "\n")
	for _, want := range []string{"--session-id", "qoder-session-1", "--resume", "--cwd", workspace} {
		if !contains(gotArgs, want) {
			t.Fatalf("argv %v missing %q", gotArgs, want)
		}
	}
	if contains(gotArgs, "--add-dir") {
		t.Fatalf("reconnect invented an extra root: %v", gotArgs)
	}
}

type fakePlugin struct {
	binary       string
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
	return ports.AgentAuthStatusUnknown, nil
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func wireContains(t *testing.T, path, needle string) bool {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(body), needle)
}

func waitWire(t *testing.T, path, needle string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		if wireContains(t, path, needle) {
			return
		}
		if time.Now().After(deadline) {
			body, _ := os.ReadFile(path)
			t.Fatalf("wire missing %q:\n%s", needle, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func runFakeStdio() {
	if argv := os.Getenv("QODER_ACP_ARGV"); argv != "" {
		_ = os.WriteFile(argv, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	}
	if pidPath := os.Getenv("QODER_ACP_PID"); pidPath != "" {
		f, err := os.OpenFile(pidPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(2)
		}
		_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
		_ = f.Close()
	}
	logPath := os.Getenv("QODER_ACP_WIRE_LOG")
	var logFile *os.File
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(2)
		}
		logFile = f
		defer logFile.Close()
	}
	logLine := func(line []byte) {
		if logFile == nil {
			return
		}
		_, _ = logFile.Write(append(append([]byte{}, line...), '\n'))
		_ = logFile.Sync()
	}
	note := func(text string) {
		logLine([]byte(text))
	}

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	var pendingPrompt json.RawMessage
	prompts := 0
	for in.Scan() {
		line := append([]byte{}, in.Bytes()...)
		logLine(line)
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Method == "" && len(msg.ID) > 0 && string(msg.ID) == `"perm-1"` && len(pendingPrompt) > 0 {
			note("prompt-result")
			_ = writeJSON(out, map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(pendingPrompt),
				"result":  map[string]any{"stopReason": "end_turn"},
			})
			pendingPrompt = nil
			continue
		}
		if len(msg.ID) == 0 || string(msg.ID) == "null" {
			continue
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": 1,
				"agentInfo":       map[string]any{"name": "qoder-cli", "version": "1.1.85"},
				"agentCapabilities": map[string]any{
					"loadSession": true,
					"sessionCapabilities": map[string]any{
						"resume":                map[string]any{},
						"additionalDirectories": map[string]any{},
					},
				},
			}
		case "session/new", "session/load", "session/resume":
			result = map[string]any{"sessionId": "qoder-session-1"}
		case "session/set_mode", "session/set_config_option":
			result = map[string]any{}
		case "_session/steering":
			result = map[string]any{"outcome": "injected"}
		case "session/prompt":
			prompts++
			if prompts == 1 {
				note("prompt-open")
				_ = writeJSON(out, map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": "qoder-session-1",
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": "live-text"},
						},
					},
				})
				_ = writeJSON(out, map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": "qoder-session-1",
						"update": map[string]any{
							"sessionUpdate": "tool_call",
							"toolCallId":    "tool-1",
							"title":         "Read file",
							"kind":          "read",
						},
					},
				})
				_ = writeJSON(out, map[string]any{
					"jsonrpc": "2.0",
					"id":      "perm-1",
					"method":  "session/request_permission",
					"params": map[string]any{
						"sessionId": "qoder-session-1",
						"toolCall":  map[string]any{"toolCallId": "tool-1", "title": "Read file", "kind": "read"},
						"options":   []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}},
					},
				})
				pendingPrompt = append(json.RawMessage(nil), msg.ID...)
				continue
			}
			note("prompt-2")
			_ = writeJSON(out, map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params": map[string]any{
					"sessionId": "qoder-session-1",
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": "follow-up"},
					},
				},
			})
			result = map[string]any{"stopReason": "end_turn"}
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
}

func writeJSON(out *bufio.Writer, v any) error {
	if err := json.NewEncoder(out).Encode(v); err != nil {
		return err
	}
	return out.Flush()
}
