package systeminstall

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// qoderAdvisoryRecorder captures the argv the fake command runner saw so a
// test can assert AO invoked only the read-only `update --check` form.
type qoderAdvisoryRecorder struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *qoderAdvisoryRecorder) record(argv []string) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), argv...))
	r.mu.Unlock()
}

func (r *qoderAdvisoryRecorder) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	for i, call := range r.calls {
		out[i] = append([]string(nil), call...)
	}
	return out
}

// newQoderAdvisoryService is the minimal harness an advisory pass needs for
// TargetQoder: the verifier resolves the binary and reports installedVersion,
// and the fake command runner answers only `qoder update --check`. Any other
// argv makes the test fail, which is how the suite proves AO never invokes the
// mutating `qoder update` form.
func newQoderAdvisoryService(t *testing.T, installedVersion, updateCheckOutput string, updateCheckErr error) (*Service, *qoderAdvisoryRecorder) {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "qodercli.exe")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho stub\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := newTestService("windows")
	s.verifier = harnessVerifierFunc(func(context.Context, Target) (VerifyResult, error) {
		return VerifyResult{ResolvedPath: binary, Output: installedVersion}, nil
	})
	recorder := &qoderAdvisoryRecorder{}
	s.commands = commandRunnerFunc(func(_ context.Context, argv []string, stdout, _ io.Writer) error {
		recorder.record(argv)
		if len(argv) >= 3 && argv[1] == "update" && argv[2] == "--check" {
			if updateCheckOutput != "" {
				_, _ = io.WriteString(stdout, updateCheckOutput)
			}
			return updateCheckErr
		}
		return errors.New("unexpected argv against qoder")
	})
	return s, recorder
}

func TestQoderAdvisoryCurrentWhenUpdateCheckReportsLatestMatch(t *testing.T) {
	s, recorder := newQoderAdvisoryService(t, "1.1.81",
		"Qoder CLI update (check only)\n\nChecking for updates...\nAlready at the latest version (1.1.81).\n", nil)
	advisory, err := s.UpdateAdvisory(context.Background(), TargetQoder, false)
	if err != nil {
		t.Fatal(err)
	}
	if advisory.Status != UpdateStatusCurrent {
		t.Fatalf("status = %q, want current (advisory=%+v)", advisory.Status, advisory)
	}
	if advisory.CurrentVersion != "1.1.81" || advisory.LatestVersion != "1.1.81" {
		t.Fatalf("versions = %q/%q, want 1.1.81/1.1.81", advisory.CurrentVersion, advisory.LatestVersion)
	}
	if advisory.Source != qoderCheckReleaseSource {
		t.Fatalf("source = %q, want %q", advisory.Source, qoderCheckReleaseSource)
	}
	// AO never installs or updates Qoder, so an advisory must not name a
	// maintenance method the settings page could interpret as "click Update".
	if advisory.MaintenanceMethod != "" {
		t.Fatalf("advisory advertised maintenance method %q for a target AO cannot install", advisory.MaintenanceMethod)
	}
	if advisory.Reason != "" {
		t.Fatalf("reason = %q, want empty on a decisive verdict", advisory.Reason)
	}
	if calls := recorder.snapshot(); len(calls) != 1 {
		t.Fatalf("probe calls = %v, want exactly one", calls)
	}
}

func TestQoderAdvisoryBehindLatestWhenUpdateCheckNamesANewerRelease(t *testing.T) {
	s, _ := newQoderAdvisoryService(t, "1.1.80",
		"Checking for updates...\nA newer release is available. The latest version (1.2.4) ships security fixes.\n", nil)
	advisory, err := s.UpdateAdvisory(context.Background(), TargetQoder, false)
	if err != nil {
		t.Fatal(err)
	}
	if advisory.Status != UpdateStatusBehindLatest || advisory.CurrentVersion != "1.1.80" || advisory.LatestVersion != "1.2.4" {
		t.Fatalf("advisory = %+v, want behind 1.1.80 -> 1.2.4", advisory)
	}
	if advisory.Source != qoderCheckReleaseSource {
		t.Fatalf("source = %q, want %q", advisory.Source, qoderCheckReleaseSource)
	}
}

// AO must never fabricate a "current" verdict from a partial or unknown
// response: a failed `update --check` becomes UpdateStatusUnknown with the
// LookupFailed reason, not a false "you are on the latest version".
func TestQoderAdvisoryUnknownOnFailureOrUnrecognizedOutput(t *testing.T) {
	for _, tc := range []struct {
		name       string
		output     string
		runErr     error
		wantReason UpdateUnknownReason
	}{
		{name: "network failure with no stdout", output: "", runErr: errors.New("dial tcp: lookup qoder.com"), wantReason: UpdateReasonLookupFailed},
		{name: "network failure with unrecognized stdout", output: "Checking for updates...\n", runErr: errors.New("network unreachable"), wantReason: UpdateReasonLookupFailed},
		{name: "clean exit but no version", output: "All good.\n", wantReason: UpdateReasonVersionUnparseable},
		{name: "clean exit but malformed version", output: "Already at the latest version (unknown).\n", wantReason: UpdateReasonVersionUnparseable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newQoderAdvisoryService(t, "1.1.81", tc.output, tc.runErr)
			advisory, err := s.UpdateAdvisory(context.Background(), TargetQoder, false)
			if err != nil {
				t.Fatal(err)
			}
			if advisory.Status != UpdateStatusUnknown {
				t.Fatalf("status = %q, want unknown (advisory=%+v)", advisory.Status, advisory)
			}
			if advisory.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", advisory.Reason, tc.wantReason)
			}
			// The installed version is still shown: only the update verdict
			// is unknown. It comes from the verifier's --version output,
			// which succeeded independently of the failing update check.
			if advisory.CurrentVersion != "1.1.81" {
				t.Fatalf("current = %q, want the version read by the verifier", advisory.CurrentVersion)
			}
			if advisory.LatestVersion != "" {
				t.Fatalf("latest = %q, want empty on an unknown verdict", advisory.LatestVersion)
			}
		})
	}
}

// The probe is bounded: a wedged `qoder update --check` cannot pin an advisory
// pass. The service surfaces a timeout as unknown, never as "current".
func TestQoderAdvisoryUnknownWhenUpdateCheckTimesOut(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "qodercli.exe")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := qoderUpdateCheckProbeTimeout
	qoderUpdateCheckProbeTimeout = 40 * time.Millisecond
	t.Cleanup(func() { qoderUpdateCheckProbeTimeout = original })

	s := newTestService("windows")
	s.verifier = harnessVerifierFunc(func(context.Context, Target) (VerifyResult, error) {
		return VerifyResult{ResolvedPath: binary, Output: "1.1.81"}, nil
	})
	s.commands = commandRunnerFunc(func(ctx context.Context, _ []string, _, _ io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	})
	advisory, err := s.UpdateAdvisory(context.Background(), TargetQoder, false)
	if err != nil {
		t.Fatal(err)
	}
	if advisory.Status != UpdateStatusUnknown || advisory.Reason != UpdateReasonLookupFailed {
		t.Fatalf("advisory = %+v, want unknown/lookup_failed", advisory)
	}
}

// A regression that re-shapes the argv is caught here: the probe must be
// exactly `<binary> update --check`, never a bare `update` or a mutating form.
func TestQoderAdvisoryUsesOnlyReadOnlyCheckArgs(t *testing.T) {
	s, recorder := newQoderAdvisoryService(t, "1.1.81", "Already at the latest version (1.1.81).\n", nil)
	if _, err := s.UpdateAdvisory(context.Background(), TargetQoder, false); err != nil {
		t.Fatal(err)
	}
	calls := recorder.snapshot()
	if len(calls) != 1 {
		t.Fatalf("probe calls = %d, want exactly one: %v", len(calls), calls)
	}
	argv := calls[0]
	if len(argv) != 3 || argv[1] != "update" || argv[2] != "--check" {
		t.Fatalf("argv = %v, want [<binary> update --check]", argv)
	}
}

func TestQoderParseLatestVersionAcceptsDocumentedShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want string
		ok   bool
	}{
		{name: "documented already-latest", out: "Qoder CLI update (check only)\n\nChecking for updates...\nAlready at the latest version (1.1.81).\n", want: "1.1.81", ok: true},
		{name: "latest version colon form", out: "The latest version: 1.2.0", want: "1.2.0", ok: true},
		{name: "latest version with prerelease", out: "You are on the latest version 1.2.0-rc.1.", want: "1.2.0-rc.1", ok: true},
		{name: "carriage returns and ANSI", out: "Checking...\r\n\x1b[2KAlready at the latest version (1.1.81).\r\n", want: "1.1.81", ok: true},
		{name: "no latest version phrase", out: "Update available: 1.1.81 -> 1.2.0", ok: false},
		{name: "empty body", out: "", ok: false},
		{name: "malformed version", out: "latest version (unknown)", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := qoderParseLatestVersion(tc.out)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (parsed %q)", ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Fatalf("version = %q, want %q", got, tc.want)
			}
		})
	}
}

// The Target registration must stay read-only: IsAgentTarget passes so the
// advisory HTTP route works, but the plan table refuses install/update/reinstall.
func TestQoderAgentTargetIsAdvisoryOnly(t *testing.T) {
	if !IsAgentTarget(TargetQoder) {
		t.Fatal("TargetQoder must be an agent target so /agents/qoder/update-advisory resolves")
	}
	if IsSystemTarget(TargetQoder) {
		t.Fatal("TargetQoder must not appear on the legacy /system/install route")
	}
	if _, ok := vendorUpdateCommands[TargetQoder]; ok {
		t.Fatal("TargetQoder must not be listed as a vendor self-updater; AO never runs the mutating `qoder update`")
	}
	planner := requestPlanner{Service: newTestService("windows")}
	for _, operation := range []AgentOperation{AgentOperationInstall, AgentOperationReinstall, AgentOperationUpdate} {
		plans := planner.agentMethodPlans(TargetQoder, operation)
		if len(plans) != 1 {
			t.Fatalf("%s plans = %v, want exactly one unsupported manual plan", operation, plans)
		}
		if !plans[0].Unsupported || plans[0].Method != "manual" {
			t.Fatalf("%s plan = %+v, want unsupported manual", operation, plans[0])
		}
	}
	installPlans := planner.agentMethodPlans(TargetQoder, AgentOperationInstall)
	if installPlans[0].DocsURL != agentDocumentationURLs[TargetQoder] || installPlans[0].DocsURL == "" {
		t.Fatalf("DocsURL = %q, want %q", installPlans[0].DocsURL, agentDocumentationURLs[TargetQoder])
	}
}
