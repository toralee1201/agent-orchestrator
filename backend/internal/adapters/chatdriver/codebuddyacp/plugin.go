package codebuddyacp

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/binaryutil"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// plugin finds the user's CodeBuddy CLI. Auth is left unknown: CodeBuddy's
// ACP handshake is the authority, and this package does not read credentials.
type plugin struct{}

func newPlugin() plugin { return plugin{} }

func (plugin) ResolveBinary(ctx context.Context) (string, error) {
	return binaryutil.ResolveBinary(ctx, binaryutil.BinarySpec{
		Label:    "codebuddy",
		Names:    []string{"codebuddy", "cbc"},
		WinNames: []string{"codebuddy.exe", "codebuddy.cmd", "cbc.exe", "cbc.cmd", "codebuddy", "cbc"},
		WinPaths: []binaryutil.WinPath{
			{Base: binaryutil.WinLocalAppData, Parts: []string{"codebuddy", "bin", "codebuddy.exe"}},
		},
	})
}

func (plugin) AuthStatus(context.Context) (ports.AgentAuthStatus, error) {
	return ports.AgentAuthStatusUnknown, nil
}
