import { render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import AppThemeProvider from "./AppThemeProvider.jsx";

// matchMedia has no jsdom implementation; stub it so useMediaQuery gets a
// deterministic answer per test instead of throwing.
function stubMatchMedia(prefersDark) {
  window.matchMedia = (query) => ({
    matches: query === "(prefers-color-scheme: dark)" && prefersDark,
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
  });
}

describe("AppThemeProvider", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  afterEach(() => {
    document.documentElement.removeAttribute("data-theme");
  });

  it("resolves auto to the OS preference and tags <html> with it", () => {
    stubMatchMedia(true);
    render(<AppThemeProvider><div>content</div></AppThemeProvider>);

    expect(screen.getByText("content")).toBeInTheDocument();
    expect(document.documentElement.dataset.theme).toBe("dark");
  });

  it("resolves auto to light when the OS does not prefer dark", () => {
    stubMatchMedia(false);
    render(<AppThemeProvider><div>content</div></AppThemeProvider>);

    expect(document.documentElement.dataset.theme).toBe("light");
  });

  it("an explicit stored mode overrides the OS preference", () => {
    localStorage.setItem("grain.themeMode", "dark");
    stubMatchMedia(false);
    render(<AppThemeProvider><div>content</div></AppThemeProvider>);

    expect(document.documentElement.dataset.theme).toBe("dark");
  });
});
