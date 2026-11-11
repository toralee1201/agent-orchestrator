// Package codebuddy supervises the CodeBuddy Code interactive CLI.
//
// TUI sessions launch `codebuddy` (alias `cbc`) with no --acp, --print, or
// --serve. AO session roles are not CodeBuddy --agent personas. Chat mode is
// not registered here: the chat worker enables it by adding a driver in
// backend/internal/adapters/chatdriver/registry.Build so
// Registry.SupportsChat(domain.HarnessCodeBuddy) becomes true. That driver
// should pass granted extra roots through AppendGrantedDirectories.
package codebuddy

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/agentbase"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/binaryutil"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const adapterID = "codebuddy"

// ModelIDs are the model ids `codebuddy --help` reported for --model.
// Launch does not pick one of these when AO leaves the model unset.
var ModelIDs = []string{
	"hy4free",
	"hy4std",
	"hy3",
	"hy3-x",
	"deepseek-v4.1-flash",
	"glm-5.3",
	"glm-5.3-flash",
	"glm-5.2",
	"space-bunny",
	"minimax-m3.1-flash",
	"minimax-m2.7",
	"kimi-k3.1-pro",
	"kimi-k3-1",
	"kimi-k2.8-preview",
	"deepseek-v4-pro",
	"step-5-preview",
	"mimo-v2.6-pro",
}

// EffortLevels are the values `codebuddy --effort` accepts.
var EffortLevels = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

var codebuddyBinarySpec = binaryutil.BinarySpec{
	Label:    "codebuddy",
	Names:    []string{"codebuddy", "cbc"},
	WinNames: []string{"codebuddy.exe", "codebuddy.cmd", "codebuddy", "cbc.exe", "cbc.cmd", "cbc"},
	UnixPaths: []string{
		"/usr/local/bin/codebuddy",
		"/opt/homebrew/bin/codebuddy",
		"/usr/local/bin/cbc",
		"/opt/homebrew/bin/cbc",
	},
	UnixHomePaths: [][]string{{".local", "bin", "codebuddy"}, {".local", "bin", "cbc"}},
	WinPaths: []binaryutil.WinPath{
		{Base: binaryutil.WinLocalAppData, Parts: []string{"codebuddy", "bin", "codebuddy.exe"}},
		{Base: binaryutil.WinLocalAppData, Parts: []string{"codebuddy", "bin", "codebuddy.cmd"}},
		{Base: binaryutil.WinLocalAppData, Parts: []string{"codebuddy", "bin", "cbc.exe"}},
		{Base: binaryutil.WinLocalAppData, Parts: []string{"codebuddy", "bin", "cbc.cmd"}},
	},
}

// versionLine matches `codebuddy --version` (2.182.0).
var versionLine = regexp.MustCompile(`(?m)^v?(\d+\.\d+\.\d+)(?:\s|$)`)

// Plugin is the CodeBuddy TUI adapter.
type Plugin struct {
	agentbase.Base
	binaryMu       sync.Mutex
	resolvedBinary string
}

// New returns a ready-to-register CodeBuddy adapter.
func New() *Plugin { return &Plugin{} }

// NewWithBinary returns an adapter that uses path without searching PATH.
// Tests use it to pin command construction.
func NewWithBinary(path string) *Plugin { return &Plugin{resolvedBinary: path} }

var _ adapters.Adapter = (*Plugin)(nil)
var _ ports.Agent = (*Plugin)(nil)

// Manifest returns the adapter's static self-description.
func (*Plugin) Manifest() adapters.Manifest {
	return adapters.Manifest{
		ID:           adapterID,
		Name:         "CodeBuddy",
		Description:  "Run CodeBuddy Code interactive TUI sessions.",
		Version:      "0.0.1",
		Capabilities: []adapters.Capability{adapters.CapabilityAgent},
	}
}

// GetConfigSpec reports the optional model and reasoning-effort overrides.
func (*Plugin) GetConfigSpec(ctx context.Context) (ports.ConfigSpec, error) {
	return agentbase.ModelEffortConfigSpec(ctx,
		"Model override passed to `codebuddy --model`. Empty omits the flag.",
		"Reasoning effort passed to `codebuddy --effort` when set: minimal, low, medium, high, xhigh, max.",
	)
}

// GetLaunchCommand builds an interactive `codebuddy` argv. The initial task is
// delivered after startup so a prompt that begins with "-" is not a flag.
func (p *Plugin) GetLaunchCommand(ctx context.Context, cfg ports.LaunchConfig) ([]string, error) {
	binary, err := p.binary(ctx)
	if err != nil {
		return nil, err
	}
	return buildCommand(binary, commandInputs{
		Permissions:      cfg.Permissions,
		Config:           cfg.Config,
		SystemPrompt:     cfg.SystemPrompt,
		SystemPromptFile: cfg.SystemPromptFile,
		SessionID:        strings.TrimSpace(cfg.NativeSessionID),
	})
}

// GetPromptDeliveryStrategy reports that AO injects the task after the TUI is up.
func (*Plugin) GetPromptDeliveryStrategy(ctx context.Context, _ ports.LaunchConfig) (ports.PromptDeliveryStrategy, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return ports.PromptDeliveryAfterStart, nil
}

// PromptReadinessHints waits for the CodeBuddy banner before the first inject.
func (*Plugin) PromptReadinessHints(ctx context.Context, _ ports.LaunchConfig) (ports.PromptReadinessHints, error) {
	if err := ctx.Err(); err != nil {
		return ports.PromptReadinessHints{}, err
	}
	return ports.PromptReadinessHints{
		InitialDelay: 750 * time.Millisecond,
		Patterns:     []string{"CodeBuddy"},
		PollInterval: 200 * time.Millisecond,
		Timeout:      8 * time.Second,
		Lines:        80,
	}, nil
}

// GetRestoreCommand continues a CodeBuddy conversation when AO has its native id.
func (p *Plugin) GetRestoreCommand(ctx context.Context, cfg ports.RestoreConfig) ([]string, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	agentSessionID := strings.TrimSpace(cfg.Session.Metadata[ports.MetadataKeyAgentSessionID])
	if agentSessionID == "" {
		return nil, false, nil
	}
	binary, err := p.binary(ctx)
	if err != nil {
		return nil, false, err
	}
	cmd, err := buildCommand(binary, commandInputs{
		Permissions:      cfg.Permissions,
		Config:           cfg.Config,
		SystemPrompt:     cfg.SystemPrompt,
		SystemPromptFile: cfg.SystemPromptFile,
		SessionID:        agentSessionID,
		Resume:           true,
	})
	if err != nil {
		return nil, false, err
	}
	return cmd, true, nil
}

// SessionInfo surfaces metadata captured by AO's generic session machinery.
func (*Plugin) SessionInfo(ctx context.Context, session ports.SessionRef) (ports.SessionInfo, bool, error) {
	if err := ctx.Err(); err != nil {
		return ports.SessionInfo{}, false, err
	}
	info, ok := agentbase.StandardSessionInfo(session)
	return info, ok, nil
}

// ResolveBinary resolves `codebuddy` or `cbc`, including the Windows native install.
func (p *Plugin) ResolveBinary(ctx context.Context) (string, error) {
	return p.binary(ctx)
}

// SupportsTerminalVersionRefresh opts into idle restarts that resume the native session.
func (*Plugin) SupportsTerminalVersionRefresh() bool { return true }

var _ ports.AgentTerminalVersionRefresher = (*Plugin)(nil)

// InvalidateBinaryResolution forgets the cached executable path.
func (p *Plugin) InvalidateBinaryResolution() {
	p.binaryMu.Lock()
	p.resolvedBinary = ""
	p.binaryMu.Unlock()
}

func (p *Plugin) binary(ctx context.Context) (string, error) {
	p.binaryMu.Lock()
	defer p.binaryMu.Unlock()
	if p.resolvedBinary != "" {
		return p.resolvedBinary, nil
	}
	binary, err := binaryutil.ResolveBinary(ctx, codebuddyBinarySpec)
	if err != nil {
		return "", err
	}
	p.resolvedBinary = binary
	return binary, nil
}

type commandInputs struct {
	Permissions      ports.PermissionMode
	Config           ports.AgentConfig
	SystemPrompt     string
	SystemPromptFile string
	SessionID        string
	Resume           bool
	Directories      []string
}

func buildCommand(binary string, in commandInputs) ([]string, error) {
	cmd := []string{binary, "--permission-mode", permissionMode(in.Permissions)}
	agentbase.AppendModelFlag(&cmd, in.Config, "--model")
	if err := appendEffort(&cmd, in.Config.Effort); err != nil {
		return nil, err
	}
	prompt, err := standingPrompt(in.SystemPrompt, in.SystemPromptFile)
	if err != nil {
		return nil, err
	}
	if prompt != "" {
		cmd = append(cmd, "--append-system-prompt", prompt)
	}
	cmd = AppendGrantedDirectories(cmd, in.Directories)
	if id := strings.TrimSpace(in.SessionID); id != "" {
		cmd = append(cmd, "--session-id", id)
		if in.Resume {
			cmd = append(cmd, "--resume", id)
		}
	}
	return cmd, nil
}

// AppendGrantedDirectories appends `--add-dir` for each directory AO has
// already granted. An empty list adds nothing.
func AppendGrantedDirectories(cmd, directories []string) []string {
	for _, directory := range directories {
		directory = strings.TrimSpace(directory)
		if directory == "" {
			continue
		}
		cmd = append(cmd, "--add-dir", directory)
	}
	return cmd
}

func permissionMode(mode ports.PermissionMode) string {
	switch ports.NormalizePermissionMode(mode) {
	case ports.PermissionModeAcceptEdits:
		return "acceptEdits"
	case ports.PermissionModeAuto:
		return "auto"
	case ports.PermissionModeBypassPermissions:
		return "bypassPermissions"
	default:
		return "default"
	}
}

func appendEffort(cmd *[]string, effort string) error {
	effort = strings.TrimSpace(effort)
	if effort == "" {
		return nil
	}
	for _, allowed := range EffortLevels {
		if effort == allowed {
			*cmd = append(*cmd, "--effort", effort)
			return nil
		}
	}
	return fmt.Errorf("codebuddy: unsupported effort %q", effort)
}

func standingPrompt(inline, path string) (string, error) {
	if text := strings.TrimSpace(inline); text != "" {
		return text, nil
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("codebuddy: inspect system prompt file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("codebuddy: system prompt file %q is not a regular file", path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is host-owned launch config
	if err != nil {
		return "", fmt.Errorf("codebuddy: read system prompt file: %w", err)
	}
	return string(data), nil
}

// ParseVersion reads a CodeBuddy `--version` banner. 2.182.0 is the observed shape.
func ParseVersion(output string) (string, bool) {
	match := versionLine.FindStringSubmatch(output)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// ReadVersion runs `path --version` and returns the parsed version.
func ReadVersion(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cmd := aoprocess.CommandContext(ctx, path, "--version")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("codebuddy: read version: %w", err)
	}
	version, ok := ParseVersion(string(out))
	if !ok {
		return "", fmt.Errorf("codebuddy: unrecognized version %q", strings.TrimSpace(string(out)))
	}
	return version, nil
}
