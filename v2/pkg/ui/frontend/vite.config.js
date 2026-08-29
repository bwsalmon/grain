import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Builds straight into ../static, the directory pkg/ui.Server embeds
// with //go:embed -- so `npm run build` here is the one required
// pre-step before `go build` in v2/, and the daemon still ships as a
// single binary with this output baked in.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../static",
    // False, and left to the `frontend` Makefile target instead: Vite's
    // own emptying would also delete ../static/.gitignore and
    // .gitkeep, the two files that make go:embed compile against an
    // otherwise-untracked, generated directory on a fresh checkout.
    emptyOutDir: false,
  },
  server: {
    // Lets `npm run dev` proxy API calls to a real `grain daemon`
    // running on its default UI port, so the frontend can be iterated
    // on with hot reload without rebuilding the Go binary.
    proxy: {
      "/api": "http://localhost:8420",
    },
  },
  // Vitest reads this same config rather than a separate file, since
  // there is nothing about the test run (component tree, module
  // resolution) that should differ from the one `vite build` uses.
  // jsdom is the one addition build/dev never needed: a DOM for
  // Testing Library to render components into without a real browser.
  test: {
    environment: "jsdom",
    setupFiles: ["./src/setupTests.js"],
  },
});
