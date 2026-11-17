package agyacp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/agy"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// TestLiveAgyAcpInitialize performs the one short handshake that admits this
// binding: resolve the installed CLI (PATH or the Windows install directory),
// start `agy acp` over stdio NDJSON, send ACP initialize, and read the
// agentInfo name/version. It skips when the CLI is absent, so CI without agy
// still passes. It writes no Google/Gemini credentials.
func TestLiveAgyAcpInitialize(t *testing.T) {
	binary, err := agy.New().ResolveBinary(context.Background())
	if err != nil {
		t.Skipf("agy CLI is not installed: %v", err)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("agy CLI binary %q is not present: %v", binary, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "acp")
	cmd.Dir = t.TempDir()
	// Isolated data dir: the handshake must not touch the operator's real
	// ~/.gemini or write any Google/Gemini credentials.
	cmd.Env = append(os.Environ(), "GEMINI_HOME="+t.TempDir())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start `agy acp`: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	request := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": 1, "clientCapabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "ao-agyacp-test", "version": "0.0.0"},
		},
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal initialize: %v", err)
	}
	if _, err := stdin.Write(append(encoded, '\n')); err != nil {
		t.Fatalf("write initialize: %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var info *acpsdk.Implementation
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var envelope struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
			Result struct {
				AgentInfo *acpsdk.Implementation `json:"agentInfo"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			continue
		}
		// Skip server->client requests/notifications (they have a method).
		if envelope.Method != "" || envelope.ID == nil {
			continue
		}
		if envelope.Result.AgentInfo != nil {
			info = envelope.Result.AgentInfo
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read initialize response: %v", err)
	}
	if info == nil {
		t.Fatal("initialize response had no agentInfo; live handshake failed")
	}
	t.Logf("live `agy acp` initialize: agentInfo.name=%q version=%q", info.Name, info.Version)
	if err := validateInitialize(acpsdk.InitializeResponse{AgentInfo: info}); err != nil {
		t.Fatalf("live server not admitted by validateInitialize: %v", err)
	}
}

// TestLiveAgyAppliesTheRequestedModel starts `agy acp` through this binding and
// checks that the model AO was asked for reaches the agent. `agy acp` has no
// --model flag, so the model can only travel as the session's own model config
// option; without it the session silently stays on Agy's default model.
func TestLiveAgyAppliesTheRequestedModel(t *testing.T) {
	binary, err := agy.New().ResolveBinary(context.Background())
	if err != nil {
		t.Skipf("agy CLI is not installed: %v", err)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("agy CLI binary %q is not present: %v", binary, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	driver := newDriver(agy.New(), nil)
	conv, err := driver.Start(ctx, ports.ChatStartConfig{
		SessionID: "agy-live-model", DataDir: t.TempDir(), WorkspacePath: t.TempDir(),
		Model: "gemini-4-hydrogen-high", Permissions: ports.PermissionModeAcceptEdits,
		// An isolated data dir: the session must not touch the operator's real
		// ~/.gemini or write any Google/Gemini credentials.
		Env: map[string]string{"GEMINI_HOME": t.TempDir()},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if stopper, ok := conv.(interface{ Terminate() error }); ok {
			_ = stopper.Terminate()
		}
	})
	configurer, ok := conv.(ports.ChatConfigOptionController)
	if !ok {
		t.Fatal("conversation does not expose the session's config options")
	}
	options, err := configurer.ListConfigOptions(ctx)
	if err != nil {
		t.Fatalf("ListConfigOptions: %v", err)
	}
	// Agy reports the model family it runs and the thinking level it derived
	// from the requested id: gemini-4-hydrogen-high becomes gemini-4-hydrogen
	// with thought_level "high".
	model, thought := "", ""
	for _, option := range options {
		switch option.ID {
		case "model":
			model = option.Current.Select
		case "thought_level":
			thought = option.Current.Select
		}
	}
	if model == "" {
		t.Fatal("live session advertised no model option")
	}
	if model != "gemini-4-hydrogen" {
		t.Fatalf("live model = %q, want gemini-4-hydrogen", model)
	}
	if thought != "high" {
		t.Fatalf("live thought level = %q, want high", thought)
	}
}
