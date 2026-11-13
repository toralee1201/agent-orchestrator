//go:build windows

package review

import (
	"context"
	"os/exec"
	"syscall"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// unitTestShellCommand runs one project shell command through cmd.exe, the same
// way Setup steps run. cmd.exe needs shell text, not Go's Windows argv
// escaping, and /s removes the enclosing quotes even when the command starts
// with a quoted path.
func unitTestShellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := aoprocess.CommandContext(ctx, "cmd")
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CmdLine = syscall.EscapeArg(cmd.Path) + ` /d /s /c "` + command + `"`
	return cmd
}
