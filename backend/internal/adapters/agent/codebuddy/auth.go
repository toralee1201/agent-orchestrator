package codebuddy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/authprobe"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ ports.AgentAuthChecker = (*Plugin)(nil)

// AuthStatus reports whether CodeBuddy Code has a local credential. CodeBuddy
// signs in through its own CLI (AO never writes its settings), and its gateway
// password in ~/.codebuddy/settings.json is the only local evidence AO reads:
// its presence is reported, its value never is. A stored credential reads as
// authorized on the strength of the local store, the same advisory level as
// DeepSeek Harness' credential file; spawn remains the validation point.
// No settings file, no gateway section, or an unreadable file stays unknown.
func (p *Plugin) AuthStatus(ctx context.Context) (ports.AgentAuthStatus, error) {
	if _, err := p.ResolveBinary(ctx); err != nil {
		return ports.AgentAuthStatusUnknown, err
	}
	if err := ctx.Err(); err != nil {
		return ports.AgentAuthStatusUnknown, err
	}
	path, err := codebuddySettingsPath()
	if err != nil || path == "" {
		return ports.AgentAuthStatusUnknown, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // adapter-owned settings path, contents never returned
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ports.AgentAuthStatusUnknown, nil
		}
		return ports.AgentAuthStatusUnknown, err
	}
	if codebuddySettingsHasCredential(data) {
		ports.RecordAuthEvidence(ctx, authprobe.FileSource(path).WithMethod(domain.AgentAccountMethodAccount))
		return ports.AgentAuthStatusAuthorized, nil
	}
	return ports.AgentAuthStatusUnknown, nil
}

// codebuddySettingsPath resolves ~/.codebuddy/settings.json without following
// any CodeBuddy-controlled override, so a hostile config cannot point AO at a
// file outside the user's home.
func codebuddySettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", err
	}
	return filepath.Join(home, ".codebuddy", "settings.json"), nil
}

// codebuddySettingsHasCredential decodes only the presence of a gateway
// credential. The password value is dropped with the rest of the document.
func codebuddySettingsHasCredential(data []byte) bool {
	var settings struct {
		Gateway struct {
			Password string `json:"password"`
		} `json:"gateway"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return false
	}
	return strings.TrimSpace(settings.Gateway.Password) != ""
}
