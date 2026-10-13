import { mkdtemp, readFile, readdir } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, describe, expect, test } from "vitest";
import { DEFAULT_POSTHOG_PROJECT_KEY } from "./posthog-config";
import { DEFAULT_SENTRY_DSN } from "./sentry-config";
import {
	buildTelemetryBootstrap,
	daemonTelemetryEnv,
	defaultDataDir,
	loadOrCreateTelemetryInstallId,
	parseDisabledEvents,
	rendererTelemetryEnabled,
} from "./telemetry";

const tempDirs: string[] = [];
const enabledPolicy = { eventsEnabled: true, consentGeneration: "7f80c8a9-ec67-4a16-a067-a444ffcc5cca", updatedAt: "2026-08-28T10:15:30.000Z", acknowledged: true, consentRenewalRequired: false };

afterEach(async () => {
	await Promise.all(
		tempDirs
			.splice(0)
			.map((dir) => import("node:fs/promises").then(({ rm }) => rm(dir, { recursive: true, force: true }))),
	);
});

test("defaultDataDir prefers AO_DATA_DIR", () => {
	expect(defaultDataDir("linux", { AO_DATA_DIR: "/tmp/custom" }, "/home/test")).toBe("/tmp/custom");
});

test("loadOrCreateTelemetryInstallId persists a stable install id", async () => {
	const dir = await mkdtemp(path.join(os.tmpdir(), "ao-telemetry-"));
	tempDirs.push(dir);

	const first = await loadOrCreateTelemetryInstallId(dir);
	const second = await loadOrCreateTelemetryInstallId(dir);
	const stored = (await readFile(path.join(dir, "telemetry_install_id"), "utf8")).trim();

	expect(first).toMatch(/^ins_/);
	expect(second).toBe(first);
	expect(stored).toBe(first);
});

test("buildTelemetryBootstrap returns null when no home dir is available", async () => {
	await expect(buildTelemetryBootstrap({}, "1.2.3", "linux", "", true, enabledPolicy)).resolves.toBeNull();
});

test("renderer telemetry is off on unpackaged builds unless explicitly opted in", () => {
	expect(rendererTelemetryEnabled({}, false)).toBe(false);
	expect(rendererTelemetryEnabled({}, true)).toBe(true);
	expect(rendererTelemetryEnabled({ AO_TELEMETRY_RENDERER: " ON " }, false)).toBe(true);
	expect(rendererTelemetryEnabled({ AO_TELEMETRY_RENDERER: "off" }, true)).toBe(false);
});

// A null bootstrap is the switch itself: initTelemetry bails on it, so an
// unpackaged build never constructs a PostHog client at all.
test("buildTelemetryBootstrap withholds the bootstrap on an unpackaged build", async () => {
	const dir = await mkdtemp(path.join(os.tmpdir(), "ao-telemetry-"));
	tempDirs.push(dir);

	const disabled = await buildTelemetryBootstrap({}, "0.11.2", "linux", dir, false, enabledPolicy);
	expect(disabled).toBeNull();
	const optedIn = await buildTelemetryBootstrap({ AO_TELEMETRY_RENDERER: "on" }, "0.11.2", "linux", dir, false, enabledPolicy);
	expect(optedIn?.appVersion).toBe("0.11.2");
});

test("no opt-in means no bootstrap, no SDK init and no install id on disk", async () => {
	const dir = await mkdtemp(path.join(os.tmpdir(), "ao-telemetry-"));
	tempDirs.push(dir);
	// Not opted in, no policy at all, or an unacknowledged opt-in: all withheld.
	expect(await buildTelemetryBootstrap({}, "0.11.2", "linux", dir, true, { ...enabledPolicy, eventsEnabled: false })).toBeNull();
	expect(await buildTelemetryBootstrap({}, "0.11.2", "linux", dir, true, undefined)).toBeNull();
	expect(await buildTelemetryBootstrap({}, "0.11.2", "linux", dir, true, { ...enabledPolicy, acknowledged: false })).toBeNull();
	// The AO_TELEMETRY_RENDERER=off environment veto beats an opt-in.
	expect(await buildTelemetryBootstrap({ AO_TELEMETRY_RENDERER: "off" }, "0.11.2", "linux", dir, true, enabledPolicy)).toBeNull();
	expect(await readdir(dir)).toEqual([]);
});

test("an explicit acknowledged opt-in yields a bootstrap with an install id", async () => {
	const dir = await mkdtemp(path.join(os.tmpdir(), "ao-telemetry-"));
	tempDirs.push(dir);
	const bootstrap = await buildTelemetryBootstrap({}, "0.11.2", "linux", dir, true, enabledPolicy);
	expect(bootstrap?.distinctId).toMatch(/^ins_/);
});

describe("daemonTelemetryEnv", () => {
	const leaky = {
		AO_TELEMETRY_EVENTS: "on",
		AO_TELEMETRY_REMOTE: "posthog",
		AO_TELEMETRY_POSTHOG_KEY: "phc_leak",
		AO_SENTRY_DSN: "https://k@o1.ingest.sentry.io/1",
	};

	test("is fully off without opt-in, even when the parent env carries keys", () => {
		const env = daemonTelemetryEnv(false, leaky, false, "1.0.0");
		expect(env).toMatchObject({
			AO_TELEMETRY_EVENTS: "off",
			AO_TELEMETRY_METRICS: "off",
			AO_TELEMETRY_REMOTE: "off",
			AO_TELEMETRY_POSTHOG_KEY: "",
			AO_SENTRY_DSN: "",
		});
	});

	test("with opt-in but no key or DSN configured, exporters stay no-ops", () => {
		const env = daemonTelemetryEnv(true, {}, false, "1.0.0");
		expect(env.AO_TELEMETRY_REMOTE).toBe("off");
		expect(env.AO_TELEMETRY_POSTHOG_KEY).toBe("");
		expect(env.AO_SENTRY_DSN).toBe("");
	});

	test("with opt-in and an explicit key/DSN, the exporters are configured", () => {
		const env = daemonTelemetryEnv(true, leaky, false, "1.0.0");
		expect(env.AO_TELEMETRY_REMOTE).toBe("posthog");
		expect(env.AO_TELEMETRY_POSTHOG_KEY).toBe("phc_leak");
		expect(env.AO_SENTRY_DSN).toBe(leaky.AO_SENTRY_DSN);
	});
});

test("the fork ships no baked PostHog key or Sentry DSN", () => {
	expect(DEFAULT_POSTHOG_PROJECT_KEY).toBe("");
	expect(DEFAULT_SENTRY_DSN).toBe("");
});

test("buildTelemetryBootstrap carries the deny list across the process boundary", async () => {
	const dir = await mkdtemp(path.join(os.tmpdir(), "ao-telemetry-"));
	tempDirs.push(dir);

	const bootstrap = await buildTelemetryBootstrap(
		{ AO_TELEMETRY_DISABLED_EVENTS: "ao.v2.app.active, ao.renderer.* ,, " },
		"0.11.2",
		"linux",
		dir,
		true,
		enabledPolicy,
	);
	expect(bootstrap?.disabledEvents).toEqual(["ao.v2.app.active", "ao.renderer.*"]);
});

test("parseDisabledEvents treats absent or blank policy as no policy", () => {
	expect(parseDisabledEvents(undefined)).toEqual([]);
	expect(parseDisabledEvents(" , , ")).toEqual([]);
});
