// Package kiroacp binds the user's own Kiro CLI to AO's reusable ACP Chat
// transport.
//
// Chat launches `kiro-cli acp` and keeps that single process for the session,
// speaking newline-delimited JSON-RPC over stdio. The one-shot
// `kiro-cli chat --output-format stream-json` print stream is not used. Kiro's
// top-level help does not list acp, but the installed CLI (3.2.1) exposes it
// as a persistent ACP agent. Login, models, and settings stay with that
// installation: AO neither downloads the CLI nor writes Kiro credentials.
package kiroacp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/nativeacp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// agentInfoName is the agentInfo.name the live `kiro-cli acp` initialize
// handshake reports. Admitting only that server keeps a wrong binary (or the
// one-shot `chat` stream) from being wired into Chat.
const agentInfoName = "Kiro CLI Agent"

// New launches `kiro-cli acp` from the exact binary resolved by the shipped
// Kiro agent plugin. There is no command override and no download.
func New(log *slog.Logger) ports.ChatDriver {
	return newDriver(newPlugin(), log)
}

func newDriver(plugin nativeacp.Plugin, log *slog.Logger) ports.ChatDriver {
	return nativeacp.New(plugin, nativeacp.Config{
		Harness:            domain.HarnessKiro,
		Configure:          configure,
		ValidateInitialize: validateInitialize,
		SessionPreamble:    sessionPreamble,
		// Kiro runs as a shell agent whose process cwd is the session workspace;
		// it does not advertise ACP additionalDirectories support, so additional
		// roots are reachable from that cwd without being sent over the wire.
		ImplicitWorkspaceDirectories: true,
		// SessionMode is deliberately nil. Kiro's advertised session modes are
		// agent personas (kiro_default, kiro_planner, kiro_guide), not AO
		// approval modes. There is no approval mode to map to, so AO skips
		// session/set_mode entirely rather than sending an unoffered mode and
		// failing startup. AO approvals are answered client-side over the
		// standard ACP session/request_permission flow instead.
		Capabilities: ports.ChatCapabilities{
			ports.ChatCapabilityApprovals: true,
		},
	}, log)
}

// configure builds the argv for the persistent ACP server. `acp` is a
// subcommand, not a flag. Kiro has no --cwd, --append-system-prompt, or
// --permission-mode flags; the transport sets the child's cwd to the workspace
// and standing instructions ride in the first prompt's text preamble.
//
// --agent, --model, --effort, --trust-all-tools and --trust-tools are v2-engine
// only and force the v2 engine, so none are passed: the live v3 handshake
// already expresses AO permissions over ACP. --auth-method keeps its `cli`
// default (the handshake succeeds without it). No Kiro credentials are sent.
func configure(_ context.Context, _ acpdriver.LaunchConfig) ([]string, map[string]string, error) {
	return []string{"acp"}, nil, nil
}

// validateInitialize admits the server the live handshake reported. Version is
// intentionally not gated: the name is the identity check, matching qoderacp.
func validateInitialize(resp acpsdk.InitializeResponse) error {
	if resp.AgentInfo == nil || resp.AgentInfo.Name != agentInfoName {
		return fmt.Errorf("unexpected ACP agent info: %+v", resp.AgentInfo)
	}
	return nil
}

// sessionPreamble leads the first prompt with AO's standing instructions as a
// plain text block. The initialize handshake reports promptCapabilities with
// embeddedContext:false, so a text content block is the channel Kiro follows;
// there is no system-prompt launch flag to use instead. Only the standing
// instructions are sent — never provider credentials or private keys.
func sessionPreamble(cfg acpdriver.LaunchConfig) []acpsdk.ContentBlock {
	standing := strings.TrimSpace(cfg.SystemPrompt)
	if standing == "" {
		return nil
	}
	return []acpsdk.ContentBlock{acpdriver.StandingInstructionsBlock(standing)}
}
