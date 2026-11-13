package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/agentlaunch"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// unitTestOutputLimit bounds the test output kept for the failure message so a
// noisy suite cannot grow daemon memory or an API error body.
const unitTestOutputLimit = 8192

// runUnitTests runs the project's unit-test command in the worker's worktree and
// returns nil only when it exits 0. The daemon, not the worker, decides whether
// the tests pass: nothing is recorded before this returns nil.
//
// The worktree HEAD must still be the head under test. A pass recorded for a
// commit the worktree is not on would let a trigger review an untested head.
func (s *Service) runUnitTests(ctx context.Context, workspacePath, command, sha string, secretEnv map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, domain.DefaultScriptStepTimeoutMinutes*time.Minute)
	defer cancel()

	if err := unitTestHeadMatches(ctx, workspacePath, sha); err != nil {
		return err
	}

	output := &boundedOutput{}
	cmd := unitTestShellCommand(ctx, command)
	aoprocess.ConfigureTreeCancellation(cmd)
	cmd.Dir = workspacePath
	cmd.Env = os.Environ()
	cmd.WaitDelay = time.Second
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	// A successful shell can leave a detached child holding its output pipes.
	// WaitDelay closes those pipes; that must not turn a passing run into a
	// failure. Real exit errors and cancellation still fail.
	if errors.Is(err, exec.ErrWaitDelay) && ctx.Err() == nil {
		err = nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%w: unit tests did not finish within %d minutes", ErrInvalid, domain.DefaultScriptStepTimeoutMinutes)
	}
	if err != nil {
		return fmt.Errorf("%w: unit tests failed in %s: %s", ErrInvalid, workspacePath, unitTestFailureDetail(output.String(), secretEnv))
	}
	// The suite itself can move HEAD — a script that checks out a branch or
	// commits its own output. Re-check after the run so the recorded pass can
	// only ever describe the head that was actually tested.
	if headErr := unitTestHeadMatches(ctx, workspacePath, sha); headErr != nil {
		return fmt.Errorf("%w; the worktree HEAD changed while the unit tests ran", headErr)
	}
	return nil
}

// unitTestHeadMatches checks the worktree is still on the head the worker asked
// to record. Short SHAs are accepted when they are unambiguous for the caller.
func unitTestHeadMatches(ctx context.Context, workspacePath, sha string) error {
	cmd := aoprocess.CommandContext(ctx, "git", "-C", workspacePath, "rev-parse", "--verify", "HEAD")
	aoprocess.ConfigureTreeCancellation(cmd)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%w: cannot resolve the worktree HEAD for %s", ErrInvalid, workspacePath)
	}
	head := strings.TrimSpace(string(out))
	if head == sha || (len(sha) >= 7 && strings.HasPrefix(head, sha)) {
		return nil
	}
	return fmt.Errorf("%w: worktree %s is on %s, not the requested head %s", ErrInvalid, workspacePath, head, sha)
}

// unitTestFailureDetail keeps the tail of the run's output and hides configured
// secret values before the text reaches an API error or a log.
func unitTestFailureDetail(output string, secretEnv map[string]string) string {
	detail := strings.TrimSpace(agentlaunch.RedactValues(output, secretEnv))
	if detail == "" {
		return "no output"
	}
	runes := []rune(detail)
	if len(runes) > unitTestOutputLimit {
		detail = "…" + string(runes[len(runes)-unitTestOutputLimit:])
	}
	return detail
}

// boundedOutput retains the tail of a command's output.
type boundedOutput struct{ tail []byte }

func (o *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	o.tail = append(o.tail, p...)
	if len(o.tail) > unitTestOutputLimit {
		o.tail = append([]byte(nil), o.tail[len(o.tail)-unitTestOutputLimit:]...)
	}
	return n, nil
}

func (o *boundedOutput) String() string { return string(o.tail) }
