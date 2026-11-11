package agyacp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestConfigureLaunchesTheAgyCLIAcpSubcommand(t *testing.T) {
	tests := []struct {
		name string
		cfg  acpdriver.LaunchConfig
		want []string
	}{
		{
			name: "bare acp subcommand, default permission",
			cfg:  acpdriver.LaunchConfig{WorkspacePath: `C:\work`},
			want: []string{"acp"},
		},
		{
			name: "default keeps approvals",
			cfg:  acpdriver.LaunchConfig{WorkspacePath: `C:\work`, Permissions: ports.PermissionModeDefault},
			want: []string{"acp"},
		},
		{
			name: "accept edits keeps approvals",
			cfg:  acpdriver.LaunchConfig{WorkspacePath: `C:\work`, Permissions: ports.PermissionModeAcceptEdits},
			want: []string{"acp"},
		},
		{
			name: "auto yolo",
			cfg:  acpdriver.LaunchConfig{WorkspacePath: `C:\work`, Permissions: ports.PermissionModeAuto},
			want: []string{"acp", "--dangerously-skip-permissions"},
		},
		{
			name: "bypass yolo",
			cfg:  acpdriver.LaunchConfig{WorkspacePath: `C:\work`, Permissions: ports.PermissionModeBypassPermissions},
			want: []string{"acp", "--dangerously-skip-permissions"},
		},
		{
			name: "additional directories only use --add-dir",
			cfg: acpdriver.LaunchConfig{
				WorkspacePath: `C:\work`, Model: "gemini-4-argon", Effort: "high",
				SystemPrompt: "Follow AO rules.", Permissions: ports.PermissionModeBypassPermissions,
				AdditionalDirectories: []string{` C:\extra `, "", `D:\other`},
			},
			want: []string{
				"acp", "--dangerously-skip-permissions",
				"--add-dir", `C:\extra`, "--add-dir", `D:\other`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, env, err := configure(context.Background(), tt.cfg)
			if err != nil {
				t.Fatalf("configure: %v", err)
			}
			if env != nil {
				t.Fatalf("configure returned env %v, want none (CLI needs no overlays)", env)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("argv = %#v, want %#v", got, tt.want)
			}
			// `agy acp --help` does not list these. Do not invent them.
			for _, banned := range []string{
				"--model", "--cwd", "--permission-mode", "--append-system-prompt",
				"--mode", "--system-prompt", "--uid=", "-p", "--print",
			} {
				for _, arg := range got {
					if arg == banned {
						t.Fatalf("argv contains undocumented flag %q: %v", banned, got)
					}
				}
			}
			if got[0] != "acp" {
				t.Fatalf("first argv must be the acp subcommand, got %q", got[0])
			}
		})
	}
}

func TestSessionModeUsesOnlyAdvertisedAcpModeIDs(t *testing.T) {
	// `agy acp` session/new advertises default/accept-edits/auto/plan. Bypass is
	// covered by --dangerously-skip-permissions on launch, so sessionMode returns
	// "" and startup skips session/set_mode instead of failing on an unoffered id.
	for mode, want := range map[ports.PermissionMode]string{
		ports.PermissionModeDefault:           "default",
		ports.PermissionModeAcceptEdits:       "accept-edits",
		ports.PermissionModeAuto:              "auto",
		ports.PermissionModeBypassPermissions: "",
	} {
		if got := sessionMode(mode); got != want {
			t.Errorf("sessionMode(%q) = %q, want %q", mode, got, want)
		}
	}
	// Old separate-server ids must never be emitted: the CLI rejects them with
	// -32602, which would fail startup.
	for _, banned := range []string{"yolo", "auto_edit"} {
		for _, mode := range []ports.PermissionMode{
			ports.PermissionModeDefault, ports.PermissionModeAcceptEdits,
			ports.PermissionModeAuto, ports.PermissionModeBypassPermissions,
		} {
			if got := sessionMode(mode); got == banned {
				t.Fatalf("sessionMode(%q) emitted old id %q", mode, banned)
			}
		}
	}
}

func TestSessionPreambleCarriesStandingInstructionsAsText(t *testing.T) {
	if got := sessionPreamble(acpdriver.LaunchConfig{SystemPrompt: "  "}); got != nil {
		t.Fatalf("blank prompt preamble = %v", got)
	}
	blocks := sessionPreamble(acpdriver.LaunchConfig{SystemPrompt: "Follow AO worker rules."})
	if len(blocks) != 1 || blocks[0].Text == nil {
		t.Fatalf("preamble = %#v, want one text block", blocks)
	}
	if !strings.Contains(blocks[0].Text.Text, "<ao_standing_instructions>\nFollow AO worker rules.\n</ao_standing_instructions>") {
		t.Fatalf("preamble text = %q", blocks[0].Text.Text)
	}
}

func TestValidateInitializeAdmitsTheAgyCLIServer(t *testing.T) {
	info := func(name, version string) acpsdk.InitializeResponse {
		return acpsdk.InitializeResponse{AgentInfo: &acpsdk.Implementation{Name: name, Version: version}}
	}
	for _, ok := range []acpsdk.InitializeResponse{
		info("Antigravity ACP Server", "1.5.5"),
		info("Antigravity ACP Server", "1.3.0"),
		info("Antigravity ACP Server", "2.0.0-beta"),
	} {
		if err := validateInitialize(ok); err != nil {
			t.Errorf("validateInitialize(%s %s) = %v", ok.AgentInfo.Name, ok.AgentInfo.Version, err)
		}
	}
	for _, bad := range []acpsdk.InitializeResponse{
		{},
		info("cline", "3.0.0"),
		info("antigravity-acp", "1.3.0"), // old separate server is no longer admitted
		info("Antigravity ACP Server", "1.2.1"),
		info("Antigravity ACP Server", "dev"),
	} {
		if err := validateInitialize(bad); err == nil {
			t.Errorf("validateInitialize(%+v) admitted an untested server", bad.AgentInfo)
		}
	}
}

func TestProbeRespectsAnUnauthorizedPlugin(t *testing.T) {
	driver := newDriver(&fakePlugin{status: ports.AgentAuthStatusUnauthorized}, nil)
	if _, err := driver.Probe(context.Background()); !errors.Is(err, ports.ErrChatAuthRequired) {
		t.Fatalf("Probe = %v, want ErrChatAuthRequired", err)
	}
	driver = newDriver(&fakePlugin{status: ports.AgentAuthStatusAuthorized}, nil)
	if _, err := driver.Probe(context.Background()); err != nil {
		t.Fatalf("Probe = %v", err)
	}
}

func TestProbeReportsAMissingCLIAsUnavailable(t *testing.T) {
	driver := newDriver(&fakePlugin{status: ports.AgentAuthStatusAuthorized, binErr: true}, nil)
	if _, err := driver.Probe(context.Background()); !errors.Is(err, ports.ErrChatDriverUnavailable) {
		t.Fatalf("Probe = %v, want ErrChatDriverUnavailable", err)
	}
}

// TestFakeProcessStartsOneAgyAcpProcess drives the whole ACP transport against
// a fake `agy acp` stdio child. It proves argv is the acp subcommand on the agy
// CLI (not agy_acp_server), that one process is kept for the session, and that
// no undocumented flags or startup model pushes reach the wire.
func TestFakeProcessStartsOneAgyAcpProcess(t *testing.T) {
	dir := t.TempDir()
	argvPath := dir + "/argv.txt"
	wirePath := dir + "/wire.log"
	pidPath := dir + "/pid.txt"
	plugin := &fakePlugin{binary: os.Args[0], status: ports.AgentAuthStatusAuthorized}
	driver := newDriver(plugin, nil)
	conv, err := driver.Start(context.Background(), ports.ChatStartConfig{
		SessionID: "agy-fake", DataDir: t.TempDir(), WorkspacePath: t.TempDir(),
		Permissions: ports.PermissionModeAcceptEdits, SystemPrompt: "standing",
		AdditionalDirectories: []string{dir + "/extra"},
		Env: map[string]string{
			"AGY_ACP_FAKE": "1", "AGY_ACP_ARGV": argvPath,
			"AGY_ACP_WIRE_LOG": wirePath, "AGY_ACP_PID": pidPath,
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
		t.Fatalf("read argv: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(argv)), "\n")
	if len(lines) == 0 || lines[0] != "acp" {
		t.Fatalf("first argv = %q, want the acp subcommand: %v", lines[0], lines)
	}
	for _, banned := range []string{
		"--model", "--cwd", "--permission-mode", "--append-system-prompt",
		"--mode", "--system-prompt", "--uid=",
	} {
		if containsArg(lines, banned) {
			t.Fatalf("argv contains undocumented flag %q: %v", banned, lines)
		}
	}
	if !containsArg(lines, "--add-dir") {
		t.Fatalf("argv missing --add-dir: %v", lines)
	}
	waitWire(t, wirePath, "initialize")
	waitWire(t, wirePath, "session/new")
	if wireContains(t, wirePath, "session/set_config_option") {
		t.Fatal("startup pushed a session config option (model); model must be advertised-only")
	}
	pids, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pid: %v", err)
	}
	if got := strings.Count(strings.TrimSpace(string(pids)), "\n") + 1; got != 1 {
		t.Fatalf("expected one kept agy acp process, got %d pids: %s", got, pids)
	}
}

type fakePlugin struct {
	binary  string
	status  ports.AgentAuthStatus
	binErr  bool
	resolve int
}

func (p *fakePlugin) ResolveBinary(context.Context) (string, error) {
	p.resolve++
	if p.binErr {
		return "", ports.ErrAgentBinaryNotFound
	}
	if p.binary == "" {
		return `C:\agy\bin\agy.exe`, nil
	}
	return p.binary, nil
}

func (p *fakePlugin) AuthStatus(context.Context) (ports.AgentAuthStatus, error) {
	return p.status, nil
}

func containsArg(items []string, want string) bool {
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

// runFakeStdio is the fake `agy acp` child. It writes its argv and pid for the
// test, then answers the ACP handshake and session/new. It advertises no
// top-level models field, so no startup model setter fires.
func runFakeStdio() {
	if argv := os.Getenv("AGY_ACP_ARGV"); argv != "" {
		_ = os.WriteFile(argv, []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	}
	if pidPath := os.Getenv("AGY_ACP_PID"); pidPath != "" {
		f, err := os.OpenFile(pidPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(2)
		}
		_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
		_ = f.Close()
	}
	logPath := os.Getenv("AGY_ACP_WIRE_LOG")
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

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
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
		if msg.Method == "" || len(msg.ID) == 0 || string(msg.ID) == "null" {
			continue
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": 1,
				"agentInfo":       map[string]any{"name": "Antigravity ACP Server", "version": "1.5.5"},
				"agentCapabilities": map[string]any{
					"loadSession": true,
					"sessionCapabilities": map[string]any{
						"resume":                map[string]any{},
						"additionalDirectories": map[string]any{},
					},
				},
			}
		case "session/new", "session/load", "session/resume":
			result = map[string]any{"sessionId": "agy-session-1"}
		case "session/set_mode", "session/set_config_option":
			result = map[string]any{}
		case "session/prompt":
			result = map[string]any{"stopReason": "end_turn"}
		default:
			_ = writeJSON(out, map[string]any{
				"jsonrpc": "2.0", "id": msg.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			})
			continue
		}
		_ = writeJSON(out, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}
}

func writeJSON(out *bufio.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := out.Write(append(data, '\n')); err != nil {
		return err
	}
	return out.Flush()
}
