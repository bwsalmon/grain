import { defineConfig } from "@playwright/test";

// PORT is fixed, not ephemeral: webServer below has to be told a URL to
// poll before any test runs, so the server needs to be listening
// somewhere known in advance. 8421 rather than 8420 -- grain demo's own
// default, and the address vite.config.js's dev proxy targets -- so this
// suite's own throwaway server never collides with one a developer
// already has running locally.
const PORT = process.env.GRAIN_E2E_PORT || 8421;
const baseURL = `http://127.0.0.1:${PORT}`;

// e2e is the one suite in this tree that is not jsdom (vite.config.js's
// own `test` block, which vitest reads): a real Chromium, driven by
// Playwright, against a real pkg/ui.Server over a real embedded SQLite
// store -- see webServer below and e2e/tasks.spec.js's own doc comment
// for why (bwsalmon/agents#415).
export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  retries: 0,
  reporter: "list",
  use: {
    baseURL,
    trace: "retain-on-failure",
  },
  webServer: {
    // `grain demo`: a real pkg/ui.Server over a real embedded SQLite
    // store in a throwaway temp directory, serving the real built
    // frontend out of ../pkg/ui/static (populated by `npm run build` /
    // `make frontend`, which has to have already run for go:embed to
    // have anything real to compile in) -- built for exactly this,
    // trying the frontend out against a real server with no daemon,
    // orchestrator, sandbox or GitHub behind it. -open=false: nothing
    // here has a display to hand a browser to.
    command: `go run ../cmd/grain demo -addr 127.0.0.1:${PORT} -open=false`,
    url: `${baseURL}/api/config`,
    reuseExistingServer: false,
    timeout: 60_000,
  },
});
