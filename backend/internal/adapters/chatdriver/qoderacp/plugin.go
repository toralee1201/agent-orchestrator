package qoderacp

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/binaryutil"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// plugin finds the user's Qoder CLI. The native qodercli executable is preferred
// over the qoder.cmd shim: cmd.exe cannot carry a multiline --append-system-prompt.
// Auth stays unknown; the ACP handshake is the authority.
type plugin struct{}

func newPlugin() plugin { return plugin{} }

func (plugin) ResolveBinary(ctx context.Context) (string, error) {
	return binaryutil.ResolveBinary(ctx, binaryutil.BinarySpec{
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
	})
}

func (plugin) AuthStatus(context.Context) (ports.AgentAuthStatus, error) {
	return ports.AgentAuthStatusUnknown, nil
}
