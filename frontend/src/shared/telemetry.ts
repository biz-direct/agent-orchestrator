import { mkdir, readFile, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { randomUUID } from "node:crypto";
import type { TelemetryPolicySnapshot } from "./telemetry-policy";
import { DEFAULT_POSTHOG_HOST, DEFAULT_POSTHOG_PROJECT_KEY } from "./posthog-config";
import { DEFAULT_SENTRY_DSN } from "./sentry-config";

export type TelemetryBootstrap = {
	distinctId: string;
	appVersion: string;
	platform: NodeJS.Platform;
	/**
	 * Event streams the renderer must not send, from
	 * `AO_TELEMETRY_DISABLED_EVENTS`.
	 *
	 * The daemon enforces the same list on its own billed sink, but renderer
	 * events go straight to PostHog and never pass through it. Without carrying
	 * the policy across the boundary, denying `ao.v2.app.active` would silence
	 * the CLI producer while the renderer kept sending under the same exported
	 * name, which is a kill switch that only half works.
	 */
	disabledEvents: string[];
	eventsEnabled: boolean;
	consentGeneration: string;
};

/** Parses the comma-separated deny list. Never throws: an unusable entry is inert. */
export function parseDisabledEvents(raw: string | undefined): string[] {
	if (!raw) return [];
	return raw
		.split(",")
		.map((name) => name.trim())
		.filter((name) => name !== "");
}

/**
 * Whether the renderer may export to PostHog.
 *
 * Unpackaged builds are off by default so a developer's ordinary session does
 * not land in the production project as a real install. Setting
 * `AO_TELEMETRY_RENDERER=on` opts a dev build back in for deliberate testing.
 */
export function rendererTelemetryEnabled(
	env: Record<string, string | undefined>,
	isPackaged: boolean,
): boolean {
	const explicit = env.AO_TELEMETRY_RENDERER?.trim().toLowerCase();
	if (explicit === "on") return true;
	if (explicit === "off") return false;
	return isPackaged;
}

export function defaultDataDir(
	platform: NodeJS.Platform,
	env: Record<string, string | undefined>,
	homeDir: string,
): string | null {
	void platform;
	if (env.AO_DATA_DIR) return env.AO_DATA_DIR;
	if (!homeDir) return null;
	return path.join(homeDir, ".ao", "data");
}

export async function loadOrCreateTelemetryInstallId(dataDir: string): Promise<string> {
	const file = path.join(dataDir, "telemetry_install_id");
	try {
		const existing = (await readFile(file, "utf8")).trim();
		if (existing) return existing;
	} catch {
		// Create the id on first use.
	}
	await mkdir(dataDir, { recursive: true });
	const distinctId = `ins_${randomUUID()}`;
	await writeFile(file, `${distinctId}\n`, { mode: 0o600 });
	return distinctId;
}

export async function buildTelemetryBootstrap(
	env: Record<string, string | undefined>,
	appVersion: string,
	platform: NodeJS.Platform,
	homeDir = os.homedir(),
	isPackaged = true,
	policy?: TelemetryPolicySnapshot,
): Promise<TelemetryBootstrap | null> {
	// Returning null is the whole switch: initTelemetry() bails on a null
	// bootstrap, so the renderer never constructs a PostHog client at all. That
	// is stronger than gating each capture, because it also removes the SDK's
	// own background requests.
	//
	// Fork policy: nothing initializes without an explicit, durably
	// acknowledged opt-in. The install id file is not even created until then.
	if (!rendererTelemetryEnabled(env, isPackaged)) return null;
	if (!policy || !policy.eventsEnabled || !policy.acknowledged) return null;
	const dataDir = defaultDataDir(platform, env, homeDir);
	if (!dataDir) return null;
	const eventsEnabled = true;
	return {
		distinctId: await loadOrCreateTelemetryInstallId(dataDir),
		appVersion,
		platform,
		disabledEvents: parseDisabledEvents(env.AO_TELEMETRY_DISABLED_EVENTS),
		eventsEnabled,
		consentGeneration: policy.consentGeneration,
	};
}

/**
 * Environment stamped on the daemon at spawn. Single policy point for every
 * daemon-side exporter (PostHog events, Sentry): without an explicit user
 * opt-in everything is forced off, even if the parent environment carries
 * keys. Once opted in, explicit env still wins; with no key/DSN configured
 * the exporters stay no-ops. Takes effect for a daemon spawned after the
 * opt-in (next launch).
 */
export function daemonTelemetryEnv(
	optedIn: boolean,
	env: Record<string, string | undefined>,
	isDev: boolean,
	appVersion: string,
): Record<string, string> {
	if (!optedIn) {
		return {
			AO_TELEMETRY_EVENTS: "off",
			AO_TELEMETRY_METRICS: "off",
			AO_TELEMETRY_REMOTE: "off",
			AO_TELEMETRY_POSTHOG_KEY: "",
			AO_SENTRY_DSN: "",
			AO_TELEMETRY_APP_VERSION: appVersion,
		};
	}
	const posthogKey = env.AO_TELEMETRY_POSTHOG_KEY ?? DEFAULT_POSTHOG_PROJECT_KEY;
	const remote = env.AO_TELEMETRY_REMOTE ?? (isDev || !posthogKey ? "off" : "posthog");
	return {
		AO_TELEMETRY_EVENTS: env.AO_TELEMETRY_EVENTS ?? "on",
		AO_TELEMETRY_REMOTE: remote,
		AO_TELEMETRY_POSTHOG_KEY: posthogKey,
		AO_TELEMETRY_POSTHOG_HOST: env.AO_TELEMETRY_POSTHOG_HOST ?? DEFAULT_POSTHOG_HOST,
		AO_SENTRY_DSN: env.AO_SENTRY_DSN ?? (isDev ? "" : DEFAULT_SENTRY_DSN),
		AO_TELEMETRY_APP_VERSION: env.AO_TELEMETRY_APP_VERSION ?? appVersion,
		...(env.AO_TELEMETRY_DISABLED_EVENTS ? { AO_TELEMETRY_DISABLED_EVENTS: env.AO_TELEMETRY_DISABLED_EVENTS } : {}),
	};
}
