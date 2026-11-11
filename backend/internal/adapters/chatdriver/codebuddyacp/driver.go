// Package codebuddyacp binds the user's own CodeBuddy CLI to AO's reusable
// ACP Chat transport.
//
// Chat launches `codebuddy --acp` over stdio NDJSON. Login, models, and
// settings stay with that installation. AO never writes CodeBuddy settings
// and never sends codebuddy.ai private credentials in ACP _meta.
package codebuddyacp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/nativeacp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// HarnessID is the chat harness key. The TUI worker owns domain registration;
// this string is the id that registration must use.
const HarnessID domain.AgentHarness = "codebuddy"

// New launches `codebuddy --acp` from the CLI resolved on PATH (or the known
// per-user install location). There is no command override and no download.
func New(log *slog.Logger) ports.ChatDriver {
	return newDriver(newPlugin(), log)
}

func newDriver(plugin nativeacp.Plugin, log *slog.Logger) ports.ChatDriver {
	return nativeacp.New(plugin, nativeacp.Config{
		Harness:   HarnessID,
		Configure: configure,
		// The CLI flag fixes the launch mode. SessionMode keeps a later AO
		// approval change on the same ACP vocabulary when the agent accepts
		// session/set_mode. A missing setter is tolerated at start.
		SessionMode: sessionMode,
		Capabilities: ports.ChatCapabilities{
			ports.ChatCapabilityApprovals: true,
		},
	}, log)
}

func configure(_ context.Context, cfg acpdriver.LaunchConfig) ([]string, map[string]string, error) {
	args := []string{"--acp"}
	if mode, ok := permissionFlag(cfg.Permissions); ok {
		args = append(args, "--permission-mode", mode)
	}

	if model := strings.TrimSpace(cfg.Model); model != "" {
		args = append(args, "--model", model)
	}

	if effort := strings.TrimSpace(cfg.Effort); effort != "" {
		if !knownEffort(effort) {
			return nil, nil, fmt.Errorf("%w: CodeBuddy effort %q", ports.ErrUnsupportedEffort, effort)
		}
		args = append(args, "--effort", effort)
	}

	if prompt := strings.TrimSpace(cfg.SystemPrompt); prompt != "" {
		args = append(args, "--append-system-prompt", prompt)
	}

	if id := strings.TrimSpace(cfg.ProviderConversationID); id != "" {
		if !validSessionID(id) {
			return nil, nil, fmt.Errorf("%w: CodeBuddy session id %q must start with a letter or number", ports.ErrChatResumeFailed, id)
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

func sessionMode(permission ports.PermissionMode) string {
	mode, _ := permissionFlag(permission)
	return mode
}

func permissionFlag(permission ports.PermissionMode) (string, bool) {
	switch ports.NormalizePermissionMode(permission) {
	case ports.PermissionModeDefault:
		return "default", true
	case ports.PermissionModeAcceptEdits:
		return "acceptEdits", true
	case ports.PermissionModeAuto:
		return "auto", true
	case ports.PermissionModeBypassPermissions:
		return "bypassPermissions", true
	default:
		return "", false
	}
}

func knownEffort(effort string) bool {
	switch effort {
	case "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

func validSessionID(id string) bool {
	for _, r := range id {
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	}
	return false
}
