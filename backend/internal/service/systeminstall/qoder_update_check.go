package systeminstall

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// qoderCheckReleaseSource marks an advisory produced by Qoder's own
// `update --check` CLI probe. It is deliberately not an install method ID and
// not a network channel: AO never fetched anything, so the settings page must
// not imply a managed update path exists.
const qoderCheckReleaseSource = "qoder-update-check"

// qoderUpdateCheckProbeTimeout bounds the read-only `qoder update --check`
// CLI probe. The command starts Node, talks to Qoder's release feed, and exits
// without writing anything; a slow first launch on a fresh install can be well
// over the manager probe budget, so it gets its own ceiling.
var qoderUpdateCheckProbeTimeout = 30 * time.Second

// qoderLatestVersionPatterns each capture the release Qoder reports as
// latest. The verified shape is
//
//	Already at the latest version (1.1.81).
//
// The behind-latest shape is not yet documented; AO keeps the same
// "latest version" prefix so a future "The latest version is 1.2.0" phrasing
// is picked up unchanged, and stays honest unknown for anything else rather
// than inventing an update verdict.
var qoderLatestVersionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)latest version[^\d]*((?:\d+\.)+\d+(?:[-+][0-9A-Za-z.-]+)?)`),
}

// qoderLatestVersion runs `<binary> update --check` and returns the release
// Qoder itself names as latest. AO never invokes the mutating form. A failed
// command or a missing version in the output surfaces as an error so the
// advisory stays honestly unknown; a successful parse returns the same display
// string versionSemver uses everywhere else.
func qoderLatestVersion(ctx context.Context, commands ports.CommandRunner, binary string) (string, error) {
	if binary == "" {
		return "", errNoOfficialSource
	}
	out, runErr := runProbe(ctx, commands, qoderUpdateCheckProbeTimeout, false, []string{binary, "update", "--check"})
	if version, ok := qoderParseLatestVersion(out); ok {
		return version, nil
	}
	if runErr != nil {
		return "", fmt.Errorf("qoder update --check: %w", runErr)
	}
	return "", fmt.Errorf("%w: qoder update --check printed no latest version", errLatestUnparseable)
}

func qoderParseLatestVersion(out string) (string, bool) {
	text := terminalEscape.ReplaceAllString(out, "")
	for _, pattern := range qoderLatestVersionPatterns {
		match := pattern.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		if version, ok := versionSemver.find(match[1]); ok {
			return version.display, true
		}
	}
	return "", false
}
