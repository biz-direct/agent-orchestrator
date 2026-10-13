// Fork policy: no baked-in Sentry DSN. Upstream's DSN would send fork users'
// error data to upstream's project. A blank DSN keeps Sentry a no-op on every
// surface (renderer, Electron main, Go daemon), even after opt-in. Supply your
// own via VITE_AO_SENTRY_DSN (renderer) / AO_SENTRY_DSN (main + daemon).
export const DEFAULT_SENTRY_DSN = "";
