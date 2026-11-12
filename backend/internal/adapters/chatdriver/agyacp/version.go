package agyacp

import (
	"fmt"
	"regexp"
	"strconv"

	acpsdk "github.com/coder/acp-go-sdk"
)

// serverAgentName is the agentInfo.name the `agy acp` CLI server reports. A live
// initialize handshake against agy 1.5.5 returned "Antigravity ACP Server"; the
// old separate agy_acp_server binary reported "antigravity-acp" and is no longer
// required.
const serverAgentName = "Antigravity ACP Server"

// minimumServerVersion is the oldest `agy acp` server AO has verified. The CLI
// reports its version in agentInfo.version, so the handshake is the gate.
const minimumServerVersion = "1.3.0"

var versionPattern = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)`)

// validateInitialize admits only the ACP agent and version the live `agy acp`
// handshake identifies.
func validateInitialize(init acpsdk.InitializeResponse) error {
	if init.AgentInfo == nil || init.AgentInfo.Name != serverAgentName {
		name := ""
		if init.AgentInfo != nil {
			name = init.AgentInfo.Name
		}
		return fmt.Errorf("expected the %s server, got agent %q", serverAgentName, name)
	}
	installed, ok := parseVersion(init.AgentInfo.Version)
	if !ok {
		return fmt.Errorf("unrecognized %s version %q (Orchestrator requires %s or newer)",
			serverAgentName, init.AgentInfo.Version, minimumServerVersion)
	}
	minimum, _ := parseVersion(minimumServerVersion)
	if installed.less(minimum) {
		return fmt.Errorf("%s %s is older than Orchestrator's tested minimum %s",
			serverAgentName, init.AgentInfo.Version, minimumServerVersion)
	}
	return nil
}

type version [3]int

func parseVersion(raw string) (version, bool) {
	match := versionPattern.FindStringSubmatch(raw)
	if len(match) != 4 {
		return version{}, false
	}
	var parsed version
	for i := range parsed {
		value, err := strconv.Atoi(match[i+1])
		if err != nil {
			return version{}, false
		}
		parsed[i] = value
	}
	return parsed, true
}

func (v version) less(other version) bool {
	for i := range v {
		if v[i] != other[i] {
			return v[i] < other[i]
		}
	}
	return false
}
