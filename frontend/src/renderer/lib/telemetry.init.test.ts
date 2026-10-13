import { beforeEach, describe, expect, it, vi } from "vitest";

// initTelemetry memoizes its result in a module-scoped promise, so each test
// resets the module registry and re-mocks the heavy boundaries (posthog, the
// Electron bridge) before importing. This exercises the real init control flow.

const posthogStub = {
	init: vi.fn(),
	register: vi.fn(),
	capture: vi.fn(() => ({})),
	captureException: vi.fn(),
	addExceptionStep: vi.fn(),
	opt_in_capturing: vi.fn(),
	opt_out_capturing: vi.fn(),
	reset: vi.fn(),
};

function mockPosthog() {
	vi.doMock("posthog-js/dist/module.full.no-external", () => ({
		default: posthogStub,
		PostHog: class {},
	}));
}

describe("initTelemetry recovery", () => {
	beforeEach(() => {
		vi.resetModules();
		vi.clearAllMocks();
		// The fork bakes in no key, so init tests supply one like a self-hoster would.
		vi.stubEnv("VITE_AO_POSTHOG_KEY", "phc_test_key");
	});

	it("retries init after a transient thrown failure instead of caching it", async () => {
		let calls = 0;
		mockPosthog();
		vi.doMock("./bridge", () => ({
			aoBridge: {
				telemetry: {
					getBootstrap: vi.fn(async () => {
						calls++;
						if (calls === 1) throw new Error("transient bootstrap failure");
						return { appVersion: "1.2.3", platform: "darwin", distinctId: "ins_test", disabledEvents: [], eventsEnabled: true, consentGeneration: "generation-1" };
					}),
				},
				updateSettings: { get: vi.fn(async () => ({})) },
			},
		}));

		const { initTelemetry } = await import("./telemetry");
		// First attempt throws -> caught -> false, and the memoized promise is dropped.
		expect(await initTelemetry()).toBe(false);
		// A later call retries and succeeds, rather than the whole session staying dark.
		expect(await initTelemetry()).toBe(true);
		expect(calls).toBe(2);
	});

	it("keeps a deliberately withheld result memoized (no retry, no re-bootstrap)", async () => {
		// Null bootstrap = telemetry withheld (no key / not opted in). That is a
		// deliberate false, not an error, so it must stay cached and NOT retry.
		const getBootstrap = vi.fn(async () => null);
		mockPosthog();
		vi.doMock("./bridge", () => ({
			aoBridge: {
				telemetry: { getBootstrap },
				updateSettings: { get: vi.fn(async () => ({})) },
			},
		}));

		const { initTelemetry } = await import("./telemetry");
		expect(await initTelemetry()).toBe(false);
		expect(await initTelemetry()).toBe(false);
		// Memoized: the withheld result is not re-attempted.
		expect(getBootstrap).toHaveBeenCalledTimes(1);
	});

	it("initializes PostHog once main supplies a bootstrap (explicit opt-in)", async () => {
		mockPosthog();
		vi.doMock("./bridge", () => ({
			aoBridge: {
				telemetry: {
					getBootstrap: vi.fn(async () => ({
						appVersion: "1.2.3", platform: "darwin", distinctId: "ins_test",
						disabledEvents: [], eventsEnabled: false, consentGeneration: "generation-off",
					})),
				},
				updateSettings: { get: vi.fn(async () => ({})) },
			},
		}));

		const { applyRendererTelemetryPolicy, clearRendererTelemetryQueues, initTelemetry } = await import("./telemetry");
		expect(await initTelemetry()).toBe(true);
		expect(posthogStub.init).toHaveBeenCalledOnce();
		expect(() => clearRendererTelemetryQueues()).not.toThrow();
		expect(() => applyRendererTelemetryPolicy(false)).not.toThrow();
	});

	it("never constructs the SDK when main withholds the bootstrap (no opt-in)", async () => {
		mockPosthog();
		vi.doMock("./bridge", () => ({
			aoBridge: { telemetry: { getBootstrap: vi.fn(async () => null) }, updateSettings: { get: vi.fn(async () => ({})) } },
		}));
		const { captureRendererEvent, initTelemetry } = await import("./telemetry");
		expect(await initTelemetry()).toBe(false);
		await captureRendererEvent("ao.renderer.loaded");
		expect(posthogStub.init).not.toHaveBeenCalled();
		expect(posthogStub.capture).not.toHaveBeenCalled();
	});

	it("makes no SDK call at all when no PostHog key is configured, even if opted in", async () => {
		vi.stubEnv("VITE_AO_POSTHOG_KEY", "");
		const getBootstrap = vi.fn();
		mockPosthog();
		vi.doMock("./bridge", () => ({ aoBridge: { telemetry: { getBootstrap }, updateSettings: { get: vi.fn(async () => ({})) } } }));
		const { initTelemetry } = await import("./telemetry");
		expect(await initTelemetry()).toBe(false);
		expect(getBootstrap).not.toHaveBeenCalled();
		expect(posthogStub.init).not.toHaveBeenCalled();
	});

	it("opts the SDK out when the user turns telemetry off after having it on", async () => {
		mockPosthog();
		vi.doMock("./bridge", () => ({
			aoBridge: {
				telemetry: { getBootstrap: vi.fn(async () => ({ appVersion: "1.2.3", platform: "darwin", distinctId: "ins_test", disabledEvents: [], eventsEnabled: true, consentGeneration: "g" })) },
				updateSettings: { get: vi.fn(async () => ({})) },
			},
		}));
		const { applyRendererTelemetryPolicy, initTelemetry } = await import("./telemetry");
		expect(await initTelemetry()).toBe(true);
		applyRendererTelemetryPolicy(false);
		expect(posthogStub.opt_out_capturing).toHaveBeenCalledOnce();
	});

	it("does not init, opt in or capture when consent is revoked while init is in flight", async () => {
		mockPosthog();
		let releaseSettings!: (value: object) => void;
		const settings = new Promise<object>((resolve) => { releaseSettings = resolve; });
		vi.doMock("./bridge", () => ({
			aoBridge: {
				telemetry: { getBootstrap: vi.fn(async () => ({ appVersion: "1.2.3", platform: "darwin", distinctId: "ins_test", disabledEvents: [], eventsEnabled: true, consentGeneration: "g" })) },
				updateSettings: { get: vi.fn(() => settings) },
			},
		}));
		const { applyRendererTelemetryPolicy, initTelemetry } = await import("./telemetry");
		const pending = initTelemetry();
		await Promise.resolve();
		applyRendererTelemetryPolicy(false);
		releaseSettings({});
		expect(await pending).toBe(false);
		expect(posthogStub.init).not.toHaveBeenCalled();
		expect(posthogStub.opt_in_capturing).not.toHaveBeenCalled();
		expect(posthogStub.capture).not.toHaveBeenCalled();
	});

	it("keeps the SDK opted out after revocation (no reset) and does not capture", async () => {
		mockPosthog();
		vi.doMock("./bridge", () => ({
			aoBridge: {
				telemetry: { getBootstrap: vi.fn().mockResolvedValueOnce({ appVersion: "1.2.3", platform: "darwin", distinctId: "ins_test", disabledEvents: [], eventsEnabled: true, consentGeneration: "g" }).mockResolvedValue(null) },
				updateSettings: { get: vi.fn(async () => ({})) },
			},
		}));
		const { applyRendererTelemetryPolicy, captureRendererEvent, initTelemetry } = await import("./telemetry");
		expect(await initTelemetry()).toBe(true);
		posthogStub.capture.mockClear();
		applyRendererTelemetryPolicy(false);
		expect(posthogStub.opt_out_capturing).toHaveBeenCalledOnce();
		expect(posthogStub.reset).not.toHaveBeenCalled();
		await captureRendererEvent("ao.renderer.route_viewed", { surface: "home" });
		expect(posthogStub.capture).not.toHaveBeenCalled();
	});
});
