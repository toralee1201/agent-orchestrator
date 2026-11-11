// Package codebuddy adapts the CodeBuddy TUI as an AO reviewer.
// The reviewer role is an AO session role. It is not passed as CodeBuddy --agent.
package codebuddy

import (
	"context"

	workercodebuddy "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/codebuddy"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Reviewer launches the same interactive CodeBuddy binary the worker uses.
type Reviewer struct {
	plugin *workercodebuddy.Plugin
}

// New returns the CodeBuddy reviewer adapter.
func New() *Reviewer { return &Reviewer{plugin: workercodebuddy.New()} }

// Harness returns CodeBuddy's reviewer identity.
func (*Reviewer) Harness() domain.ReviewerHarness { return domain.ReviewerCodeBuddy }

var _ ports.Reviewer = (*Reviewer)(nil)
var _ ports.ReviewerCanceller = (*Reviewer)(nil)

// ReviewCommand starts an interactive CodeBuddy session with AO's standing
// reviewer instructions. Permission stays on CodeBuddy's default so approval
// prompts remain. A granted task-prompt root is the only extra directory.
func (r *Reviewer) ReviewCommand(ctx context.Context, inv ports.ReviewInvocation) (ports.ReviewCommandSpec, error) {
	argv, err := r.command(ctx, inv, "")
	if err != nil {
		return ports.ReviewCommandSpec{}, err
	}
	return ports.ReviewCommandSpec{Argv: argv}, nil
}

// ReviewRestoreCommand resumes the recorded CodeBuddy conversation.
func (r *Reviewer) ReviewRestoreCommand(ctx context.Context, inv ports.ReviewInvocation) (ports.ReviewCommandSpec, bool, error) {
	if inv.AgentSessionID == "" {
		return ports.ReviewCommandSpec{}, false, nil
	}
	argv, err := r.command(ctx, inv, inv.AgentSessionID)
	if err != nil {
		return ports.ReviewCommandSpec{}, false, err
	}
	return ports.ReviewCommandSpec{Argv: argv}, true, nil
}

// ReviewCancel interrupts the current CodeBuddy turn and leaves the pane open.
func (*Reviewer) ReviewCancel(context.Context) (ports.ReviewCancelSpec, error) {
	return ports.ReviewCancelSpec{Mode: ports.ReviewCancelInterrupt, Interrupts: 2}, nil
}

// ReviewMessage returns the next AO-owned task reference.
func (*Reviewer) ReviewMessage(_ context.Context, inv ports.ReviewInvocation) (string, error) {
	return inv.Prompt, nil
}

func (r *Reviewer) command(ctx context.Context, inv ports.ReviewInvocation, sessionID string) ([]string, error) {
	plugin := r.plugin
	if plugin == nil {
		plugin = workercodebuddy.New()
	}
	var argv []string
	var err error
	if sessionID == "" {
		argv, err = plugin.GetLaunchCommand(ctx, ports.LaunchConfig{
			Config: inv.Config, Permissions: ports.PermissionModeDefault,
			SystemPrompt: inv.SystemPrompt, SystemPromptFile: inv.SystemPromptFile,
		})
	} else {
		var ok bool
		argv, ok, err = plugin.GetRestoreCommand(ctx, ports.RestoreConfig{
			Config: inv.Config, Permissions: ports.PermissionModeDefault,
			SystemPrompt: inv.SystemPrompt, SystemPromptFile: inv.SystemPromptFile,
			Session: ports.SessionRef{Metadata: map[string]string{ports.MetadataKeyAgentSessionID: sessionID}},
		})
		if err == nil && !ok {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	if inv.TaskPromptRoot != "" {
		argv = workercodebuddy.AppendGrantedDirectories(argv, []string{inv.TaskPromptRoot})
	}
	return argv, nil
}
