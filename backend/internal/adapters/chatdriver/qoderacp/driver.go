// Package qoderacp binds the user's own Qoder CLI to AO's reusable ACP Chat
// transport.
//
// Chat launches `qodercli --acp` and keeps that process for the session.
// `qoder --help` does not list --acp, but the installed CLI (1.1.85) accepts it
// and speaks newline-delimited JSON-RPC until the process is closed. One-shot
// `qoder -p` is not used. Login, models, and settings stay with that
// installation: AO never writes Qoder settings and never replaces the system
// prompt.
package qoderacp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode"

	acpsdk "github.com/coder/acp-go-sdk"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/nativeacp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// New launches `qodercli --acp` from the CLI resolved on PATH or the known
// per-user install location. There is no command override and no download.
func New(log *slog.Logger) ports.ChatDriver {
	return newDriver(newPlugin(), log)
}

func newDriver(plugin nativeacp.Plugin, log *slog.Logger) ports.ChatDriver {
	return nativeacp.New(plugin, nativeacp.Config{
		Harness:            domain.HarnessQoder,
		Configure:          configure,
		SessionMode:        sessionMode,
		ValidateInitialize: validateInitialize,
		Capabilities: ports.ChatCapabilities{
			ports.ChatCapabilityApprovals: true,
		},
	}, log)
}

func configure(_ context.Context, cfg acpdriver.LaunchConfig) ([]string, map[string]string, error) {
	args := []string{"--acp", "--cwd", cfg.WorkspacePath, "--permission-mode", permissionFlag(cfg.Permissions)}
	if model := strings.TrimSpace(cfg.Model); model != "" {
		args = append(args, "--model", model)
	}
	if effort := strings.TrimSpace(cfg.Effort); effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}
	if prompt := strings.TrimSpace(cfg.SystemPrompt); prompt != "" {
		args = append(args, "--append-system-prompt", prompt)
	}
	if id := strings.TrimSpace(cfg.ProviderConversationID); id != "" {
		if !validSessionID(id) {
			return nil, nil, fmt.Errorf("%w: Qoder session id %q must start with a letter or number", ports.ErrChatResumeFailed, id)
		}
		args = append(args, "--session-id", id, "--resume")
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

// permissionFlag uses the names `qodercli --help` documents for --permission-mode.
// Qoder also accepts dont_ask; AO has no matching permission mode, so it is not selected.
func permissionFlag(permission ports.PermissionMode) string {
	switch ports.NormalizePermissionMode(permission) {
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

// sessionMode uses the ids session/new advertises. Bypass is "yolo" on the ACP
// session even though the CLI flag is bypass_permissions.
func sessionMode(permission ports.PermissionMode) string {
	switch ports.NormalizePermissionMode(permission) {
	case ports.PermissionModeAcceptEdits:
		return "acceptEdits"
	case ports.PermissionModeAuto:
		return "auto"
	case ports.PermissionModeBypassPermissions:
		return "yolo"
	default:
		return "default"
	}
}

func validateInitialize(resp acpsdk.InitializeResponse) error {
	if resp.AgentInfo == nil || resp.AgentInfo.Name != "qoder-cli" {
		return fmt.Errorf("unexpected ACP agent info: %+v", resp.AgentInfo)
	}
	return nil
}

func validSessionID(id string) bool {
	for _, r := range id {
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	}
	return false
}
