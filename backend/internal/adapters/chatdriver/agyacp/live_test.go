package agyacp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/agy"
	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
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
//
// The model id is discovered from the installed agent's own session catalog
// instead of being hardcoded (model ids vary per install and change over time).
// The test is a live smoke check, so an unauthenticated machine or a model the
// agent refuses is skipped rather than failed: that is an environment issue,
// not a regression in this binding.
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
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Read the model catalog from a short discovery session, under an isolated
	// data dir so nothing touches the operator's real ~/.gemini credentials.
	discoveryDir := t.TempDir()
	catalog, err := acpdriver.DiscoverConfigOptionsForHarness(ctx, acpdriver.Launch{
		Command: binary,
		Args:    []string{"acp"},
		Env:     map[string]string{"GEMINI_HOME": discoveryDir},
	}, discoveryDir, domain.HarnessAgy, log)
	if err != nil {
		if skipAgyLiveError(err) {
			t.Skipf("cannot read agy session catalog (environment): %v", err)
		}
		t.Fatalf("discover agy session catalog: %v", err)
	}

	requestedModel := dynamicAgyModelID(t, catalog)
	wantFamily, wantEffort := splitAgyModelEffort(requestedModel)
	t.Logf("discovered agy model id %q (family %q, effort %q)", requestedModel, wantFamily, wantEffort)

	driver := newDriver(agy.New(), nil)
	conv, err := driver.Start(ctx, ports.ChatStartConfig{
		SessionID: "agy-live-model", DataDir: t.TempDir(), WorkspacePath: t.TempDir(),
		Model: requestedModel, Permissions: ports.PermissionModeAcceptEdits,
		// An isolated data dir: the session must not touch the operator's real
		// ~/.gemini or write any Google/Gemini credentials.
		Env: map[string]string{"GEMINI_HOME": t.TempDir()},
	})
	if err != nil {
		if skipAgyLiveError(err) {
			t.Skipf("agy session cannot use a live model (environment): %v", err)
		}
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
	if model != wantFamily {
		t.Fatalf("live model = %q, want family %q from discovered id %q", model, wantFamily, requestedModel)
	}
	if wantEffort != "" && thought != wantEffort {
		t.Fatalf("live thought level = %q, want %q from discovered id %q", thought, wantEffort, requestedModel)
	}
}

// dynamicAgyModelID builds an effort-qualified agy model id from the values the
// session actually advertises: a "model" choice for the family and a
// "thought_level" choice for the effort. Nothing about a specific model family
// or effort level is hardcoded here.
func dynamicAgyModelID(t *testing.T, catalog []ports.ChatConfigOption) string {
	t.Helper()
	var model *ports.ChatConfigOption
	for i := range catalog {
		if catalog[i].ID == "model" {
			model = &catalog[i]
			break
		}
	}
	if model == nil {
		t.Skipf("session advertises no model option to test: %#v", catalog)
	}

	// Prefer a choice that already carries a recognized effort suffix; the
	// split then comes straight from the catalog with no synthesizing.
	family, effort := "", ""
	for _, choice := range model.Choices {
		value := strings.TrimSpace(choice.Value)
		if value == "" {
			continue
		}
		family, effort = splitAgyModelEffort(value)
		if effort != "" {
			break
		}
	}
	// Some installs advertise only the current value with no choice list. Use
	// it as the family rather than skipping the smoke check.
	if family == "" {
		family, _ = splitAgyModelEffort(strings.TrimSpace(model.Current.Select))
	}
	if family == "" {
		t.Skipf("session advertises no usable model value: %#v", model)
	}

	// A family-only choice needs an effort to exercise the split. Take one the
	// session advertises and that splitAgyModelEffort recognizes.
	if effort == "" {
		for i := range catalog {
			if catalog[i].ID != "thought_level" {
				continue
			}
			for _, choice := range catalog[i].Choices {
				value := strings.TrimSpace(choice.Value)
				if value == "" {
					continue
				}
				if _, recognized := splitAgyModelEffort("model-" + value); recognized != "" {
					effort = value
					break
				}
			}
			break
		}
	}
	requested := family
	if effort != "" {
		requested = family + "-" + effort
	}
	return requested
}

// skipAgyLiveError reports whether an error is an environment condition that a
// live smoke test should skip: missing credentials or a model the agent will
// not accept. Both the ACP sentinels and the provider's raw messages are
// checked because the failure can surface either way.
func skipAgyLiveError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ports.ErrChatAuthRequired) || errors.Is(err, ports.ErrChatConfigOptionInvalid) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"authentication required", "auth_required", "unauthorized", "not authenticated",
		"invalid model", "unknown model", "model not found",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
