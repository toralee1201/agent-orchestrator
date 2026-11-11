package qoder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/authprobe"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// qoderStatusProbeTimeout bounds the read-only `qoder status --output json`
// probe. Qoder is a Node CLI like the sibling `update --check` advisory, whose
// cold start reliably exceeds a short budget: a 5-second cap timed out healthy
// signed-in installs, and the coordinator then preserved the last definite
// verdict, pinning a stale signed-out state to the settings page. The probe
// runs alongside readiness refresh under the coordinator's own per-harness
// ceiling, so it gets the same 30-second budget Hermes' adapter uses; a wedged
// CLI start still cannot stall a settings page indefinitely.
var qoderStatusProbeTimeout = 30 * time.Second

// AuthStatus reports whether Qoder is locally authenticated. The probe runs
// the CLI's own documented status command and reads only its `logged_in`
// boolean, so a real signed-out user reads as unauthorized rather than
// "unknown", and a signed-in one reads as authorized. The payload's username,
// email, avatar, account type and any credential material are never parsed
// out, logged, or returned to the caller: the classifier decodes one field
// and drops the rest.
//
// A missing binary, a probe that fails to start, a timeout, output that is not
// valid JSON, or JSON without a boolean `logged_in` all remain honest
// unknowns. AO never guesses from a heuristic or reads credential files.
func (p *Plugin) AuthStatus(ctx context.Context) (ports.AgentAuthStatus, error) {
	binary, err := p.ResolveBinary(ctx)
	if err != nil {
		return ports.AgentAuthStatusUnknown, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, qoderStatusProbeTimeout)
	defer cancel()
	out, err := authprobe.CmdRunner(probeCtx, binary, "status", "--output", "json")
	if probeCtx.Err() != nil {
		return ports.AgentAuthStatusUnknown, fmt.Errorf("qoder status probe: %w", probeCtx.Err())
	}
	status, recognized := qoderStatusFromJSON(out)
	if recognized {
		return status, nil
	}
	if err != nil {
		// A failed exit whose output AO cannot decode is not evidence of any
		// login state; report the run's error so the caller records the retry
		// rather than treating it as a signed-out verdict.
		return ports.AgentAuthStatusUnknown, fmt.Errorf("qoder status probe: %w", err)
	}
	return ports.AgentAuthStatusUnknown, nil
}

// qoderStatusFromJSON decodes exactly the `logged_in` boolean from a
// `qoder status --output json` payload. The bool reports whether the payload
// was one AO can positively classify; a false answer keeps the caller honest
// rather than falling through to a shared prose classifier that could latch
// onto the account name or plan string that shares the JSON body.
//
// The decoder targets a struct that names only `logged_in`, so every other
// field (username, email, avatar_url, account_type, user_type, auth_source,
// version, allow_byok, and any field a future build adds) is discarded while
// parsing and never reaches a diagnostic, error string, or the readiness
// account record.
func qoderStatusFromJSON(out []byte) (ports.AgentAuthStatus, bool) {
	var payload struct {
		LoggedIn *bool `json:"logged_in"`
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	// Decode only the first JSON value; trailing prose (an ANSI status line,
	// a progress bar) is not part of the payload and must not be a signal.
	if err := dec.Decode(&payload); err != nil {
		return ports.AgentAuthStatusUnknown, false
	}
	if payload.LoggedIn == nil {
		return ports.AgentAuthStatusUnknown, false
	}
	if *payload.LoggedIn {
		return ports.AgentAuthStatusAuthorized, true
	}
	return ports.AgentAuthStatusUnauthorized, true
}
