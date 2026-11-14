package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/agentlaunch"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

// unitTestOutputLimit bounds the test output kept for the failure message so a
// noisy suite cannot grow daemon memory or an API error body.
const unitTestOutputLimit = 8192

// runUnitTests runs the project's unit-test command against a clean snapshot of
// one commit and returns the resolved full commit SHA. The daemon, not the
// worker, decides whether the tests pass: nothing is recorded before this
// returns without error.
//
// The command runs in a throwaway worktree checked out at the requested commit,
// never in the worker's own directory. Modified, staged, or untracked files in
// that directory would otherwise let a suite pass on content that is not part of
// the commit under review, and the recorded pass would claim a tested commit
// that was never actually tested. The snapshot also leaves the worker's
// uncommitted files untouched.
func (s *Service) runUnitTests(ctx context.Context, project domain.ProjectRecord, workspacePath, command, sha string, secretEnv map[string]string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, domain.DefaultScriptStepTimeoutMinutes*time.Minute)
	defer cancel()

	// Resolve first: a pass must name the full commit, and a short SHA in the
	// record would never match the provider's full head SHA at trigger time.
	commit, err := resolveUnitTestCommit(ctx, workspacePath, sha)
	if err != nil {
		return "", err
	}
	// A pass describes the commit the worker is on. Recording one for a commit
	// the worktree has left would let a trigger review an untested head.
	if err := unitTestHeadMatches(ctx, workspacePath, commit); err != nil {
		return "", err
	}
	root, err := s.unitTestRoot()
	if err != nil {
		return "", err
	}
	cleanAbandonedUnitTestSnapshots(root, time.Now())
	snapshot, cleanup, err := unitTestSnapshot(ctx, workspacePath, root, commit)
	if err != nil {
		return "", err
	}
	defer cleanup()

	// The snapshot is a pristine checkout, which by itself breaks any suite that
	// needs installed dependencies or a shared .env: npm test in a fresh worktree
	// has no node_modules. Prepare it exactly like a session workspace so the
	// gate reflects whether the commit's suite passes in a ready workspace,
	// rather than failing for lack of setup the worker's own workspace has.
	if err := sessionmanager.ProvisionWorkspace(ctx, project, snapshot, nil); err != nil {
		return "", fmt.Errorf("%w: cannot prepare the unit-test snapshot: %w", ErrInvalid, err)
	}

	output := &boundedOutput{}
	cmd := unitTestShellCommand(ctx, command)
	aoprocess.ConfigureTreeCancellation(cmd)
	cmd.Dir = snapshot
	cmd.Env = os.Environ()
	cmd.WaitDelay = time.Second
	cmd.Stdout, cmd.Stderr = output, output
	err = cmd.Run()
	// A successful shell can leave a detached child holding its output pipes.
	// WaitDelay closes those pipes; that must not turn a passing run into a
	// failure. Real exit errors and cancellation still fail.
	if errors.Is(err, exec.ErrWaitDelay) && ctx.Err() == nil {
		err = nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("%w: unit tests did not finish within %d minutes", ErrInvalid, domain.DefaultScriptStepTimeoutMinutes)
	}
	if err != nil {
		return "", fmt.Errorf("%w: unit tests failed in %s: %s", ErrInvalid, workspacePath, unitTestFailureDetail(output.String(), secretEnv))
	}
	// The snapshot is pristine, but the commit the pass describes can still
	// move under it: the suite shares the object store and may repoint the ref
	// the caller named, and the session may have moved the worktree meanwhile.
	// A pass recorded now would describe a commit that is no longer the one
	// being reviewed.
	if again, revErr := resolveUnitTestCommit(ctx, workspacePath, sha); revErr != nil {
		return "", fmt.Errorf("%w; the requested commit stopped resolving while the unit tests ran", revErr)
	} else if again != commit {
		return "", fmt.Errorf("%w: the requested commit %s now resolves to %s", ErrInvalid, commit, again)
	}
	if err := unitTestHeadMatches(ctx, workspacePath, commit); err != nil {
		return "", err
	}
	return commit, nil
}

// resolveUnitTestCommit turns a caller-supplied (possibly abbreviated) commit
// into the full SHA of the commit it names.
func resolveUnitTestCommit(ctx context.Context, workspacePath, sha string) (string, error) {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return "", fmt.Errorf("%w: a commit sha is required", ErrInvalid)
	}
	cmd := aoprocess.CommandContext(ctx, "git", "-C", workspacePath, "rev-parse", "--verify", sha+"^{commit}")
	aoprocess.ConfigureTreeCancellation(cmd)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: cannot resolve %q in %s", ErrInvalid, sha, workspacePath)
	}
	full := strings.TrimSpace(string(out))
	if full == "" {
		return "", fmt.Errorf("%w: cannot resolve %q in %s", ErrInvalid, sha, workspacePath)
	}
	return full, nil
}

// unitTestSnapshot checks out commit in a temporary detached worktree under
// root and returns its path plus a cleanup that removes it. A worktree shares
// the repository's object store, so the suite can still run git commands, while
// the checkout itself is a pristine copy of exactly that commit.
func unitTestSnapshot(ctx context.Context, workspacePath, root, commit string) (string, func(), error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", nil, fmt.Errorf("%w: cannot create the snapshot root: %w", ErrInvalid, err)
	}
	dir, err := os.MkdirTemp(root, "run-")
	if err != nil {
		return "", nil, fmt.Errorf("%w: cannot create a snapshot directory: %w", ErrInvalid, err)
	}
	snapshot := filepath.Join(dir, "snapshot")
	cleanup := func() { _ = os.RemoveAll(dir) }

	add := aoprocess.CommandContext(ctx, "git", "-C", workspacePath, "worktree", "add", "--detach", snapshot, commit)
	aoprocess.ConfigureTreeCancellation(add)
	add.WaitDelay = time.Second
	if out, addErr := add.CombinedOutput(); addErr != nil {
		cleanup()
		return "", nil, fmt.Errorf("%w: cannot check out %s for the unit tests: %s", ErrInvalid, commit, strings.TrimSpace(string(out)))
	}
	cleanup = func() {
		remove := aoprocess.CommandContext(context.WithoutCancel(ctx), "git", "-C", workspacePath, "worktree", "remove", "--force", snapshot)
		remove.WaitDelay = time.Second
		_ = remove.Run()
		_ = os.RemoveAll(dir)
	}
	return snapshot, cleanup, nil
}

// unitTestRoot is where snapshots live: under the daemon's data dir, so AO-owned
// checkouts and any artifacts a suite leaves behind stay inside the state
// boundary instead of the OS temp dir.
func (s *Service) unitTestRoot() (string, error) {
	dataDir := strings.TrimSpace(s.dataDir)
	if dataDir == "" {
		return "", fmt.Errorf("%w: the review service has no data directory for the unit-test snapshot", ErrInvalid)
	}
	return filepath.Join(dataDir, "review-tests"), nil
}

// unitTestSnapshotMaxAge bounds how long a snapshot directory may survive. A run
// is capped by the unit-test timeout, so anything older cannot be in flight and
// is left over from a run that was killed (daemon restart, power loss) before
// its cleanup ran.
var unitTestSnapshotMaxAge = domain.DefaultScriptStepTimeoutMinutes * time.Minute * 2

// cleanAbandonedUnitTestSnapshots removes snapshot directories older than
// unitTestSnapshotMaxAge. A killed run never reaches its cleanup, so nothing
// else would ever remove the checkout it left behind. Best effort: a failure
// here never blocks recording a pass. The directory is removed without a
// `git worktree remove`, so the repository keeps a registration whose working
// tree is gone; git reports that as prunable and it does not block a later
// `git worktree add`.
func cleanAbandonedUnitTestSnapshots(root string, now time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || now.Sub(info.ModTime()) < unitTestSnapshotMaxAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, entry.Name()))
	}
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
