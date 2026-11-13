//go:build !windows

package review

import (
	"context"
	"os/exec"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// unitTestShellCommand runs one project shell command through the platform
// shell, the same way Setup steps run.
func unitTestShellCommand(ctx context.Context, command string) *exec.Cmd {
	return aoprocess.CommandContext(ctx, "sh", "-c", command)
}
