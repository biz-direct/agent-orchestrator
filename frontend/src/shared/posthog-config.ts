// Fork policy: no baked-in PostHog project key. Upstream's key would send fork
// users' data to upstream's project. Analytics are off by default and, with no
// key configured, a no-op even after opt-in. Supply a key explicitly
// (VITE_AO_POSTHOG_KEY / AO_TELEMETRY_POSTHOG_KEY) to point at your own project.
export const DEFAULT_POSTHOG_PROJECT_KEY = "";
export const DEFAULT_POSTHOG_HOST = "https://us.i.posthog.com";
