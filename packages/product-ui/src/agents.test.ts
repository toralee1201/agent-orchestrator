import { describe, expect, it } from "vitest";
import { AGENT_OPTIONS, agentLabel, getAgentIdentity, hasManagedInstallTarget, hasUpdateAdvisoryTarget } from "./agents";

describe("agent identities", () => {
	it("keeps OpenCode 2 a distinct worker option with its own label", () => {
		expect(AGENT_OPTIONS).toContain("opencode-v2");
		expect(agentLabel("opencode")).toBe("OpenCode");
		expect(agentLabel("opencode-v2")).toBe("OpenCode 2");
		expect(getAgentIdentity("opencode-v2")).toMatchObject({
			id: "opencode-v2",
			label: "OpenCode 2",
			logoKey: "opencode-v2",
		});
	});

	it("offers Qoder without a managed installer and still polls its version advisory", () => {
		expect(AGENT_OPTIONS.filter((agent) => agent === "qoder")).toHaveLength(1);
		expect(agentLabel("qoder")).toBe("Qoder");
		expect(getAgentIdentity("qoder")).toMatchObject({ id: "qoder", label: "Qoder", logoKey: "qoder" });
		expect(hasManagedInstallTarget("qoder")).toBe(false);
		expect(hasUpdateAdvisoryTarget("qoder")).toBe(true);
		for (const agent of AGENT_OPTIONS) {
			if (agent !== "qoder") expect(hasManagedInstallTarget(agent)).toBe(true);
			expect(hasUpdateAdvisoryTarget(agent)).toBe(true);
		}
	});
});
