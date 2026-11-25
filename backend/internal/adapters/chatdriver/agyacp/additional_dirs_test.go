package agyacp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/agy"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/binaryutil"
	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// cliLaunchArgs must grant every configured workspace root as --add-dir. The
// fork passed these on the CLI so that even an `agy acp` without ACP
// additional-directories support could open a multi-repo session.
func TestCLILaunchArgsPassAdditionalDirectories(t *testing.T) {
	got := cliLaunchArgs(map[string]string{}, func(string) string { return "" }, []string{
		"  /work/repo-a  ", "", "  ", "/work/repo-b",
	})
	want := []string{
		"acp", "--app-data-dir", agy.ACPStorageDir,
		"--add-dir", "/work/repo-a", "--add-dir", "/work/repo-b",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cliLaunchArgs = %v, want %v", got, want)
	}
}

// A server that does not advertise the additional-directories capability must
// still start a multi-repo session: the roots are granted as --add-dir CLI
// flags and marked handled in ACP setup (ImplicitWorkspaceDirectories), so
// they are not sent on the wire and do not trip the capability check.
func TestCLIStartsWithoutAdditionalDirectoriesCapability(t *testing.T) {
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	env := map[string]string{
		"AGY_ACP_FAKE":               "1",
		"AGY_ACP_ARGV":               argvPath,
		"AGY_ACP_NO_ADDITIONAL_DIRS": "1",
	}
	r := resolver{
		getenv: func(key string) string { return env[key] },
		isFile: isExecutableFile,
		agyCLI: func(context.Context) (string, error) { return os.Args[0], nil },
		cliVersion: func(context.Context, string) (string, error) {
			return "1.5.6", nil
		},
		resolveSpec: func(context.Context, binaryutil.BinarySpec) (string, error) {
			return "", errors.New("not used")
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	driver := acpdriver.New(buildConfig(nil, r, log), log)

	extra := t.TempDir()
	conv, err := driver.Start(context.Background(), ports.ChatStartConfig{
		SessionID:             "agy-no-dirs-cap",
		DataDir:               t.TempDir(),
		WorkspacePath:         t.TempDir(),
		Permissions:           ports.PermissionModeAcceptEdits,
		AdditionalDirectories: []string{extra},
		Env:                   env,
	})
	if err != nil {
		t.Fatalf("Start with a server lacking additional-directories support: %v", err)
	}
	if stopper, ok := conv.(interface{ Terminate() error }); ok {
		t.Cleanup(func() { _ = stopper.Terminate() })
	}

	argv := waitForArgvFile(t, argvPath)
	if !containsArg(argv, "--add-dir") || !containsArg(argv, extra) {
		t.Fatalf("argv missing --add-dir %q: %v", extra, argv)
	}
}

func waitForArgvFile(t *testing.T, path string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return strings.Split(strings.TrimSpace(string(data)), "\n")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for argv file %s", path)
	return nil
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
