package qoder

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/authprobe"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/binaryutil"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// binarySpecPointingAt returns a BinarySpec whose every candidate path lies
// under dir (an empty temp dir) and whose PATH names cannot resolve, so
// binaryutil.ResolveBinary fails deterministically on every developer machine
// regardless of whether Qoder CLI is installed alongside AO. Used by tests
// that must observe the missing-binary code path.
func binarySpecPointingAt(dir string) binaryutil.BinarySpec {
	return binaryutil.BinarySpec{
		Label:     "qoder",
		Names:     []string{"qoder-ao-test-binary-does-not-exist"},
		WinNames:  []string{"qoder-ao-test-binary-does-not-exist.exe"},
		UnixPaths: []string{filepath.Join(dir, "absent")},
		WinPaths: []binaryutil.WinPath{
			{Base: binaryutil.WinHome, Parts: []string{".qoder-ao-test-absent", "absent.exe"}},
		},
	}
}

// installQoderAuthRunner swaps authprobe.CmdRunner for the duration of the
// test and asserts the probe was invoked with the exact documented status
// arguments, so a regression that changes the CLI surface cannot pass silent.
func installQoderAuthRunner(t *testing.T, runner func(ctx context.Context, name string, arg ...string) ([]byte, error)) {
	t.Helper()
	previous := authprobe.CmdRunner
	t.Cleanup(func() { authprobe.CmdRunner = previous })
	authprobe.CmdRunner = func(ctx context.Context, name string, arg ...string) ([]byte, error) {
		if got, want := strings.Join(arg, " "), "status --output json"; got != want {
			t.Fatalf("probe args = %q, want %q", got, want)
		}
		return runner(ctx, name, arg...)
	}
}

// The verifier of the JSON classifier: only the logged_in field is read, so a
// payload carrying every private account field still yields a clean boolean
// status and cannot leak a username, email, avatar URL or plan name through
// the error path.
func TestQoderStatusFromJSONReadsOnlyLoggedIn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		out        string
		want       ports.AgentAuthStatus
		recognized bool
	}{
		{name: "signed in, full payload", out: `{"logged_in": true, "username": "tora lee", "email": "who@example.invalid", "avatar_url": "https://qoder.invalid/who", "user_type": "personal_professional_trial"}`, want: ports.AgentAuthStatusAuthorized, recognized: true},
		{name: "signed out, full payload", out: `{"logged_in": false, "username": "someone", "email": "who@example.invalid"}`, want: ports.AgentAuthStatusUnauthorized, recognized: true},
		{name: "signed in, only field", out: `{"logged_in": true}`, want: ports.AgentAuthStatusAuthorized, recognized: true},
		{name: "signed out, only field", out: `{"logged_in": false}`, want: ports.AgentAuthStatusUnauthorized, recognized: true},
		{name: "whitespace and trailing content", out: "\r\n {\"logged_in\": true}\r\ntrailing prose", want: ports.AgentAuthStatusAuthorized, recognized: true},
		{name: "missing logged_in", out: `{"version": "1.1.81"}`, want: ports.AgentAuthStatusUnknown, recognized: false},
		{name: "null logged_in", out: `{"logged_in": null}`, want: ports.AgentAuthStatusUnknown, recognized: false},
		{name: "string logged_in", out: `{"logged_in": "true"}`, want: ports.AgentAuthStatusUnknown, recognized: false},
		{name: "empty body", out: "", want: ports.AgentAuthStatusUnknown, recognized: false},
		{name: "prose only", out: "Logged in. Welcome back!", want: ports.AgentAuthStatusUnknown, recognized: false},
		{name: "malformed json", out: "{ not json", want: ports.AgentAuthStatusUnknown, recognized: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := qoderStatusFromJSON([]byte(tc.out))
			if got != tc.want || ok != tc.recognized {
				t.Fatalf("classify(%q) = (%v, %v), want (%v, %v)", tc.out, got, ok, tc.want, tc.recognized)
			}
		})
	}
}

func TestAuthStatusReportsAuthorizedForLoggedInProbe(t *testing.T) {
	installQoderAuthRunner(t, func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"logged_in": true, "username": "whoever", "email": "who@example.invalid"}`), nil
	})
	plugin := &Plugin{resolvedBinary: "qoder"}
	status, err := plugin.AuthStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status != ports.AgentAuthStatusAuthorized {
		t.Fatalf("status = %v, want authorized", status)
	}
}

func TestAuthStatusReportsUnauthorizedForSignedOutProbe(t *testing.T) {
	installQoderAuthRunner(t, func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"logged_in": false}`), nil
	})
	plugin := &Plugin{resolvedBinary: "qoder"}
	status, err := plugin.AuthStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status != ports.AgentAuthStatusUnauthorized {
		t.Fatalf("status = %v, want unauthorized", status)
	}
}

// A probe that returns an error but still yields a valid logged_in field is a
// decisive answer: the boolean outranks the exit code, so a CLI that prints
// its state and then exits non-zero is read as signed in or signed out rather
// than unknown.
func TestAuthStatusReadsDecisiveVerdictDespiteCommandError(t *testing.T) {
	installQoderAuthRunner(t, func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"logged_in": false}`), errors.New("exit status 3")
	})
	plugin := &Plugin{resolvedBinary: "qoder"}
	status, err := plugin.AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != ports.AgentAuthStatusUnauthorized {
		t.Fatalf("status = %v, want unauthorized", status)
	}
}

// When the probe fails and its output is not decodable, the error surfaces so
// the readiness record can retry, and the status is honest unknown rather
// than a guessed signed-out verdict.
func TestAuthStatusUnknownOnProbeError(t *testing.T) {
	installQoderAuthRunner(t, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("killed"), errors.New("signal: killed")
	})
	plugin := &Plugin{resolvedBinary: "qoder"}
	status, err := plugin.AuthStatus(context.Background())
	if err == nil {
		t.Fatal("expected the command error to surface for retry accounting")
	}
	if status != ports.AgentAuthStatusUnknown {
		t.Fatalf("status = %v, want unknown", status)
	}
}

func TestAuthStatusUnknownOnMalformedOutput(t *testing.T) {
	installQoderAuthRunner(t, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("not json at all"), nil
	})
	plugin := &Plugin{resolvedBinary: "qoder"}
	status, err := plugin.AuthStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status != ports.AgentAuthStatusUnknown {
		t.Fatalf("status = %v, want unknown", status)
	}
}

// An unavailable binary short-circuits before the probe runs, so a broken
// install never masquerades as signed out.
func TestAuthStatusUnknownWhenBinaryMissing(t *testing.T) {
	// Redirect the binary spec at an empty directory so ResolveBinary fails on
	// every developer machine, regardless of whether Qoder CLI is installed
	// alongside AO.
	previous := qoderBinarySpec
	qoderBinarySpec = binarySpecPointingAt(t.TempDir())
	t.Cleanup(func() { qoderBinarySpec = previous })

	plugin := &Plugin{}
	status, err := plugin.AuthStatus(context.Background())
	if err == nil {
		t.Fatal("expected resolve error when no binary is installed")
	}
	if status != ports.AgentAuthStatusUnknown {
		t.Fatalf("status = %v, want unknown", status)
	}
}

// A wedged CLI is bounded: the probe ctx is derived from a timeout, so the
// AuthStatus call returns an error rather than hanging a readiness pass, and
// the status is honest unknown.
func TestAuthStatusHonorsBoundedProbeTimeout(t *testing.T) {
	original := qoderStatusProbeTimeout
	qoderStatusProbeTimeout = 40 * time.Millisecond
	t.Cleanup(func() { qoderStatusProbeTimeout = original })

	installQoderAuthRunner(t, func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	plugin := &Plugin{resolvedBinary: "qoder"}
	status, err := plugin.AuthStatus(context.Background())
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want DeadlineExceeded", err)
	}
	if status != ports.AgentAuthStatusUnknown {
		t.Fatalf("status = %v, want unknown", status)
	}
}

// A caller-side cancellation propagates: the readiness coordinator must be
// able to abandon the probe when a refresh window closes.
func TestAuthStatusPropagatesCallerCancellation(t *testing.T) {
	installQoderAuthRunner(t, func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	plugin := &Plugin{resolvedBinary: "qoder"}
	status, err := plugin.AuthStatus(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if status != ports.AgentAuthStatusUnknown {
		t.Fatalf("status = %v, want unknown", status)
	}
}

// Guard against a regression that re-shapes the JSON classifier: the returned
// status must not carry account-bearing text (username, email, avatar) from
// the payload into an error message or the readiness record.
func TestQoderStatusErrorDoesNotLeakAccountFields(t *testing.T) {
	installQoderAuthRunner(t, func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"logged_in": true, "email": "secret@corp.invalid", "username": "Real Name", "avatar_url": "https://qoder.invalid/avatar"}`), nil
	})
	plugin := &Plugin{resolvedBinary: "qoder"}
	status, err := plugin.AuthStatus(context.Background())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status != ports.AgentAuthStatusAuthorized {
		t.Fatalf("status = %v, want authorized", status)
	}
	// Neither the status constant nor any error surface may carry a value
	// from the JSON payload. Assert against the raw field text: no error is
	// returned on the happy path, and the status enum is a fixed string.
	if strings.Contains(string(status), "@") || strings.Contains(string(status), "Real Name") {
		t.Fatalf("status value leaked account data: %q", string(status))
	}
}
