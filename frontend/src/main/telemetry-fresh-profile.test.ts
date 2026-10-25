import { mkdtemp, readdir, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, expect, it, vi } from "vitest";
import { buildTelemetryBootstrap, daemonTelemetryEnv } from "../shared/telemetry";
import { DesktopTelemetryController } from "./desktop-telemetry-controller";
import { TelemetryPolicyAuthority, readTelemetryOptInSync } from "./telemetry-policy-file";

const dirs: string[] = [];
afterEach(async () => {
	vi.restoreAllMocks();
	await Promise.all(dirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })));
});

// A fresh packaged install, end to end through the supervisor's telemetry
// decisions: no SDK transport is built, no bootstrap is handed to the renderer,
// the daemon is spawned fully off, and nothing touches the network.
it("a fresh packaged profile makes zero outbound telemetry requests and initializes no SDK", async () => {
	const home = await mkdtemp(path.join(os.tmpdir(), "ao-fresh-"));
	dirs.push(home);
	const dataDir = path.join(home, ".ao", "data");
	const fetchSpy = vi.spyOn(globalThis, "fetch").mockRejectedValue(new Error("network must not be touched"));

	const authority = new TelemetryPolicyAuthority({ dataDir, platform: "linux" });
	const transportFactory = vi.fn(async () => null);
	const controller = new DesktopTelemetryController({ authority, transportFactory, environmentAllowsEvents: true });
	const view = await controller.initialize();

	expect(view.eventsEnabled).toBe(false);
	expect(transportFactory).not.toHaveBeenCalled();
	expect(readTelemetryOptInSync(dataDir)).toBe(false);
	expect(await buildTelemetryBootstrap({ AO_DATA_DIR: dataDir }, "1.0.0", "linux", home, true, controller.snapshot())).toBeNull();
	expect(daemonTelemetryEnv(readTelemetryOptInSync(dataDir), {}, false, "1.0.0")).toMatchObject({
		AO_TELEMETRY_EVENTS: "off",
		AO_TELEMETRY_REMOTE: "off",
		AO_TELEMETRY_POSTHOG_KEY: "",
		AO_SENTRY_DSN: "",
	});
	// No install id is minted without consent.
	expect(await readdir(dataDir)).toEqual(["telemetry_policy.json"]);
	expect(fetchSpy).not.toHaveBeenCalled();
});
