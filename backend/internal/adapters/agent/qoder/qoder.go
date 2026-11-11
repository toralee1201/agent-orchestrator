// Package qoder supervises the Qoder interactive CLI.
//
// TUI sessions launch `qodercli` (or `qoder`) with no --acp or --print.
// AO session roles are not Qoder --agent personas. Chat mode is registered
// separately by backend/internal/adapters/chatdriver/qoderacp. That driver
// owns the ACP permission mapping, including bypass as yolo; this adapter
// only maps AO approval values onto `qodercli --permission-mode`.
package qoder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

const adapterID = "qoder"

// ModelIDs are the names `qoder --list-models` reported for Qoder CLI 1.1.85.
// Launch does not pick one of these when AO leaves the model unset.
var ModelIDs = []string{
	"Auto",
	"Ultimate",
	"Fast",
	"Efficient",
	"Sonus",
	"Cantus",
	"Qwen3.8-Max",
	"Qwen3.8-Flash",
	"Kimi-K3.1-Pro",
	"Kimi-K2.8-Preview",
	"GLM-5.3",
	"GLM-5.3-Flash",
	"DeepSeek-V4-Pro",
	"DeepSeek-Flash",
	"MiniMax-M3.1-Flash",
}

// EffortLevels are the values `qodercli --reasoning-effort` accepts.
// Qoder CLI 1.1.85 rejects anything outside this set.
var EffortLevels = []string{"auto", "none", "low", "medium", "high", "xhigh", "max", "ultracode"}

var qoderBinarySpec = binaryutil.BinarySpec{
	Label:    "qoder",
	Names:    []string{"qodercli", "qoder"},
	WinNames: []string{"qodercli.exe", "qoder.exe", "qoder.cmd"},
	WinPaths: []binaryutil.WinPath{
		{Base: binaryutil.WinHome, Parts: []string{".qoder", "bin", "qodercli", "qodercli.exe"}},
		{Base: binaryutil.WinHome, Parts: []string{".qoder", "entry", "qoder.cmd"}},
	},
	UnixHomePaths: [][]string{
		{".qoder", "bin", "qodercli", "qodercli"},
	},
}

// versionLine matches `qoder --version` (1.1.85).
var versionLine = regexp.MustCompile(`(?m)^v?(\d+\.\d+\.\d+)(?:\s|$)`)

// Plugin is the Qoder TUI adapter.
type Plugin struct {
	agentbase.Base
	binaryMu       sync.Mutex
	resolvedBinary string
}

// New returns a ready-to-register Qoder adapter.
func New() *Plugin { return &Plugin{} }

// NewWithBinary returns an adapter that uses path without searching PATH.
// Tests use it to pin command construction.
func NewWithBinary(path string) *Plugin { return &Plugin{resolvedBinary: path} }

var _ adapters.Adapter = (*Plugin)(nil)
var _ ports.Agent = (*Plugin)(nil)
var _ ports.AgentAuthChecker = (*Plugin)(nil)

// Manifest returns the adapter's static self-description.
func (*Plugin) Manifest() adapters.Manifest {
	return adapters.Manifest{
		ID:           adapterID,
		Name:         "Qoder",
		Description:  "Run Qoder interactive TUI sessions.",
		Version:      "0.0.1",
		Capabilities: []adapters.Capability{adapters.CapabilityAgent},
	}
}

// GetConfigSpec reports the optional model and reasoning-effort overrides.
func (*Plugin) GetConfigSpec(ctx context.Context) (ports.ConfigSpec, error) {
	return agentbase.ModelEffortConfigSpec(ctx,
		"Model override passed to `qodercli --model`. Empty omits the flag. Custom models use the model id.",
		"Reasoning effort passed to `qodercli --reasoning-effort` when set: auto, none, low, medium, high, xhigh, max, ultracode.",
	)
}

// GetLaunchCommand builds an interactive `qodercli` argv. The initial task is
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

// PromptReadinessHints waits for the Qoder banner before the first inject.
func (*Plugin) PromptReadinessHints(ctx context.Context, _ ports.LaunchConfig) (ports.PromptReadinessHints, error) {
	if err := ctx.Err(); err != nil {
		return ports.PromptReadinessHints{}, err
	}
	return ports.PromptReadinessHints{
		InitialDelay: 750 * time.Millisecond,
		Patterns:     []string{"Qoder"},
		PollInterval: 200 * time.Millisecond,
		Timeout:      8 * time.Second,
		Lines:        80,
	}, nil
}

// GetRestoreCommand continues a Qoder conversation when AO has its native id.
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

// AuthStatus is defined in auth.go. It runs `qoder status --output json`
// and reports only the logged_in boolean.

// ResolveBinary resolves `qodercli` or `qoder`, including the Windows native install.
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
	binary, err := binaryutil.ResolveBinary(ctx, qoderBinarySpec)
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
	if err := checkCmdLauncherTransport(binary, prompt); err != nil {
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
		return "accept_edits"
	case ports.PermissionModeAuto:
		return "auto"
	case ports.PermissionModeBypassPermissions:
		return "bypass_permissions"
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
			*cmd = append(*cmd, "--reasoning-effort", effort)
			return nil
		}
	}
	return fmt.Errorf("qoder: unsupported effort %q", effort)
}

// checkCmdLauncherTransport rejects a Windows .cmd/.bat launcher when the
// standing instructions contain a newline. cmd.exe ends the command at that
// newline and drops every flag after it. Native executables keep the argument.
func checkCmdLauncherTransport(binary, systemPrompt string) error {
	switch strings.ToLower(filepath.Ext(binary)) {
	case ".cmd", ".bat":
	default:
		return nil
	}
	if !strings.ContainsAny(systemPrompt, "\n\r") {
		return nil
	}
	return fmt.Errorf("qoder: standing instructions contain newlines, which the Windows launcher %q cannot carry through cmd.exe; install the native Qoder CLI so AO can start it directly", binary)
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
		return "", fmt.Errorf("qoder: inspect system prompt file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("qoder: system prompt file %q is not a regular file", path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is host-owned launch config
	if err != nil {
		return "", fmt.Errorf("qoder: read system prompt file: %w", err)
	}
	return string(data), nil
}

// ParseVersion reads a Qoder `--version` banner. 1.1.85 is the observed shape.
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
		return "", fmt.Errorf("qoder: read version: %w", err)
	}
	version, ok := ParseVersion(string(out))
	if !ok {
		return "", fmt.Errorf("qoder: unrecognized version %q", strings.TrimSpace(string(out)))
	}
	return version, nil
}
