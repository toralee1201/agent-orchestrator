// Package agyacp binds the user's own Agy CLI to AO's reusable ACP Chat
// transport.
//
// Chat launches `agy acp` over stdio NDJSON and keeps that one process for the
// session, the same shape as qoderacp and codebuddyacp. The ACP server ships
// with the agy CLI; AO never downloads, bundles, or substitutes a separate
// agy_acp_server. Login, models, MCP configuration, and conversations stay the
// user's own, and AO never writes Gemini or Google credentials.
package agyacp

import (
	"context"
	"log/slog"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/agy"
	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/nativeacp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Plugin is the existing Agy agent-plugin surface the binding reuses for CLI
// resolution and the canonical auth probe. The ACP server is the agy CLI itself.
type Plugin interface {
	ResolveBinary(context.Context) (string, error)
	AuthStatus(context.Context) (ports.AgentAuthStatus, error)
}

// New constructs the Agy ACP Chat driver. It launches the resolved agy binary
// as `agy acp`; there is no command override and no download. The plugin is
// the existing agy adapter, so AuthStatus stays shared with the TUI worker.
func New(plugin Plugin, log *slog.Logger) ports.ChatDriver {
	if plugin == nil {
		plugin = agy.New()
	}
	return newDriver(plugin, log)
}

func newDriver(plugin nativeacp.Plugin, log *slog.Logger) ports.ChatDriver {
	return nativeacp.New(plugin, nativeacp.Config{
		Harness:            domain.HarnessAgy,
		Configure:          configure,
		SessionMode:        sessionMode,
		ValidateInitialize: validateInitialize,
		SessionPreamble:    sessionPreamble,
		Capabilities: ports.ChatCapabilities{
			ports.ChatCapabilityApprovals: true,
		},
	}, log)
}

// configure builds the `agy acp` argv using only flags `agy acp --help` lists.
// The process cwd is set to the workspace by the ACP transport (see
// acp/process.go), so there is no --cwd flag. --dangerously-skip-permissions is
// the documented yolo flag; AO's auto and bypass tiers use it to match the old
// yolo behavior. --add-dir grants additional roots. Model, system prompt, and
// permission mode are NOT launch flags on the acp subcommand: model stays an
// ACP session option only when advertised, and standing instructions go through
// the session preamble below.
func configure(_ context.Context, cfg acpdriver.LaunchConfig) ([]string, map[string]string, error) {
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

// sessionMode maps AO's approval vocabulary onto the mode ids `agy acp`
// advertises in session/new (default, accept-edits, auto, plan). These are NOT
// the old separate server's yolo/auto_edit ids. Bypass is handled by the
// --dangerously-skip-permissions launch flag, so it returns "" and startup
// skips session/set_mode rather than failing on an unoffered mode. Keeping
// session/set_mode here lets a later AO approval change use the same ACP
// vocabulary when the agent offers the mode.
func sessionMode(permission ports.PermissionMode) string {
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

// sessionPreamble delivers AO's standing instructions as a text block ahead of
// the first prompt. `agy acp` has no system-prompt launch flag, and it treats an
// embedded resource as something to look up rather than as instructions, so
// plain text is the channel the CLI follows. The instructions do not depend on
// any mode the user can switch, so they survive approval changes, and they stay
// out of private credentials.
func sessionPreamble(cfg acpdriver.LaunchConfig) []acpsdk.ContentBlock {
	standing := strings.TrimSpace(cfg.SystemPrompt)
	if standing == "" {
		return nil
	}
	return []acpsdk.ContentBlock{acpsdk.TextBlock(
		"Standing instructions from Agent Orchestrator. Follow them for this whole session:\n" +
			"<ao_standing_instructions>\n" + standing + "\n</ao_standing_instructions>",
	)}
}
