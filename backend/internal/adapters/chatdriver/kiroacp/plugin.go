package kiroacp

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/kiro"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// plugin reuses the shipped Kiro agent adapter for both binary resolution and
// auth, so Chat and Terminal UI agree on which kiro-cli is installed and
// whether it is signed in. AO writes no Kiro credentials: AuthStatus reads the
// CLI's own `whoami` output. There is no download or bundled executable.
type plugin struct {
	agent *kiro.Plugin
}

func newPlugin() plugin { return plugin{agent: kiro.New()} }

func (p plugin) ResolveBinary(ctx context.Context) (string, error) {
	return kiro.ResolveKiroBinary(ctx)
}

func (p plugin) AuthStatus(ctx context.Context) (ports.AgentAuthStatus, error) {
	return p.agent.AuthStatus(ctx)
}
