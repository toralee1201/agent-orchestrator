package agyacp

// This file restores the fork's test-only helpers that the retained fork
// host_test.go and live_test.go depend on. Upstream v0.15.6 rewrote driver.go
// around acpdriver.Config and dropped these symbols; keeping them here avoids
// changing upstream driver semantics while the fork's live/fake-process tests
// still compile and run against the real `agy acp` transport.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/nativeacp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// newDriver is the fork's test hook: build the ACP Chat driver on the native
// ACP transport, which is what the retained fork live tests drive.
func newDriver(plugin nativeacp.Plugin, log *slog.Logger) ports.ChatDriver {
	return nativeacp.New(plugin, nativeacp.Config{
		Harness:            domain.HarnessAgy,
		Configure:          forkConfigure,
		SessionMode:        forkSessionMode,
		SessionOptions:     forkSessionOptions,
		ValidateInitialize: validateInitialize,
		SessionPreamble:    sessionPreamble,
		Capabilities: ports.ChatCapabilities{
			ports.ChatCapabilityApprovals: true,
		},
	}, log)
}

// forkSessionMode maps approval modes to the mode ids the fork's native `agy
// acp` session offered (default / accept-edits / auto), not upstream's separate
// server ids. The fork's live test drives that native transport.
func forkSessionMode(permission ports.PermissionMode) string {
	switch ports.NormalizePermissionMode(permission) {
	case ports.PermissionModeDefault:
		return "default"
	case ports.PermissionModeAcceptEdits:
		return "accept-edits"
	case ports.PermissionModeAuto:
		return "auto"
	case ports.PermissionModeBypassPermissions:
		return ""
	default:
		return "default"
	}
}

// forkSessionOptions applies the discovered model family and effort as separate
// session options, the way the fork's native transport did.
func forkSessionOptions(settings ports.ChatTurnSettings) []acpdriver.SessionOption {
	model := strings.TrimSpace(settings.Model)
	if model == "" {
		return nil
	}
	family, effort := splitAgyModelEffort(model)
	options := []acpdriver.SessionOption{{ID: "model", Value: family}}
	if effort != "" {
		options = append(options, acpdriver.SessionOption{ID: "thought_level", Value: effort})
	}
	return options
}

// forkConfigure mirrors the fork's `agy acp` argv builder. It is used only by
// the native transport that newDriver constructs for the fork tests.
func forkConfigure(_ context.Context, cfg acpdriver.LaunchConfig) ([]string, map[string]string, error) {
	args := []string{"acp"}
	if ports.NormalizePermissionMode(cfg.Permissions) == ports.PermissionModeAuto ||
		ports.NormalizePermissionMode(cfg.Permissions) == ports.PermissionModeBypassPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	for _, dir := range cfg.AdditionalDirectories {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		args = append(args, "--add-dir", dir)
	}
	return args, nil, nil
}

// splitAgyModelEffort splits an effort-qualified agy model id
// ("<family>-<effort>") into its family and thinking-level parts. This is the
// fork's suffix table, retained for the fork's live model test.
func splitAgyModelEffort(model string) (family, effort string) {
	for _, suffix := range []string{"xhigh", "medium", "high", "low"} {
		if strings.HasSuffix(model, "-"+suffix) {
			return strings.TrimSuffix(model, "-"+suffix), suffix
		}
	}
	return model, ""
}

// agyFakeModelOption and agyFakeThoughtLevelOption mirror the config options
// `agy acp` advertises in session/new, used by runFakeStdio.
var agyFakeModelOption = map[string]any{
	"id": "model", "name": "Model", "category": "model", "type": "select",
	"currentValue": "gemini-4-argon",
	"options": []any{
		map[string]any{"value": "gemini-4-argon", "name": "Gemini 4 Argon"},
		map[string]any{"value": "gemini-4-hydrogen", "name": "Gemini 4 Hydrogen"},
	},
}

var agyFakeThoughtLevelOption = map[string]any{
	"id": "thought_level", "name": "Thinking Level", "category": "thought_level", "type": "select",
	"currentValue": "medium",
	"options": []any{
		map[string]any{"value": "low", "name": "Low"},
		map[string]any{"value": "medium", "name": "Medium"},
		map[string]any{"value": "high", "name": "High"},
	},
}

// runFakeStdio is the fake `agy acp` stdio child retained from the fork. The
// fork's host_test.go re-executes this test binary with the `acp` subcommand to
// drive a full ACP conversation without a real provider.
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

	echoConfig := os.Getenv("AGY_ACP_ECHO_CONFIG") == "1"
	configValues := map[string]any{}
	if echoConfig {
		configValues["model"] = agyFakeModelOption["currentValue"]
		configValues["thought_level"] = agyFakeThoughtLevelOption["currentValue"]
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
			Params json.RawMessage `json:"params"`
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
			sessionCapabilities := map[string]any{
				"resume": map[string]any{},
			}
			// AGY_ACP_NO_ADDITIONAL_DIRS drives coverage for a server that does
			// not advertise ACP additional-directories support.
			if os.Getenv("AGY_ACP_NO_ADDITIONAL_DIRS") != "1" {
				sessionCapabilities["additionalDirectories"] = map[string]any{}
			}
			result = map[string]any{
				"protocolVersion": 1,
				"agentInfo":       map[string]any{"name": "Antigravity ACP Server", "version": "1.5.5"},
				"agentCapabilities": map[string]any{
					"loadSession":         true,
					"sessionCapabilities": sessionCapabilities,
				},
			}
		case "session/new", "session/load", "session/resume":
			result = map[string]any{"sessionId": "agy-session-1"}
			if os.Getenv("AGY_ACP_MODELS") == "1" {
				result = map[string]any{
					"sessionId": "agy-session-1",
					"configOptions": []any{
						agyFakeModelOption,
						agyFakeThoughtLevelOption,
					},
				}
			}
		case "session/set_config_option":
			result = map[string]any{}
			if echoConfig {
				var params struct {
					ConfigID string `json:"configId"`
					Value    string `json:"value"`
				}
				if err := json.Unmarshal(msg.Params, &params); err == nil && params.ConfigID != "" {
					configValues[params.ConfigID] = params.Value
				}
				modelOption := map[string]any{}
				for k, v := range agyFakeModelOption {
					modelOption[k] = v
				}
				modelOption["currentValue"] = configValues["model"]
				thoughtOption := map[string]any{}
				for k, v := range agyFakeThoughtLevelOption {
					thoughtOption[k] = v
				}
				thoughtOption["currentValue"] = configValues["thought_level"]
				result = map[string]any{
					"configOptions": []any{modelOption, thoughtOption},
				}
			}
		case "session/set_mode":
			result = map[string]any{}
		case "session/prompt":
			result = map[string]any{"stopReason": "end_turn"}
		default:
			_ = writeForkJSON(out, map[string]any{
				"jsonrpc": "2.0", "id": msg.ID,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			})
			continue
		}
		_ = writeForkJSON(out, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}
}

func writeForkJSON(out *bufio.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := out.Write(append(data, '\n')); err != nil {
		return err
	}
	return out.Flush()
}
