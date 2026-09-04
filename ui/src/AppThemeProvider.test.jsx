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
    document.getElementById("favicon")?.remove();
    document.getElementById("favicon-png")?.remove();
  });

  it("resolves auto to the OS preference and tags <html> with it", () => {
    stubMatchMedia(true);
    render(
      <AppThemeProvider>
        <div>content</div>
      </AppThemeProvider>,
    );

    expect(screen.getByText("content")).toBeInTheDocument();
    expect(document.documentElement.dataset.theme).toBe("dark");
  });

  it("resolves auto to light when the OS does not prefer dark", () => {
    stubMatchMedia(false);
    render(
      <AppThemeProvider>
        <div>content</div>
      </AppThemeProvider>,
    );

    expect(document.documentElement.dataset.theme).toBe("light");
  });

  it("an explicit stored mode overrides the OS preference", () => {
    localStorage.setItem("grain.themeMode", "dark");
    stubMatchMedia(false);
    render(
      <AppThemeProvider>
        <div>content</div>
      </AppThemeProvider>,
    );

    expect(document.documentElement.dataset.theme).toBe("dark");
  });

  // index.html ships the light mark, since it has to name something
  // before any of this has run; a reader in dark mode should end up with
  // the wheat one in the tab rather than the bronze one drawn for a
  // light ground.
  it("points the tab icon at the mark drawn for the resolved theme", () => {
    // Both links, because a browser picks whichever of the two it can
    // render and either one left on the light mark is a bronze icon in a
    // dark tab on some machines and not others.
    for (const [id, href] of [
      ["favicon", "/grain-mark-light.svg"],
      ["favicon-png", "/grain-mark-light.png"],
    ]) {
      const link = document.createElement("link");
      link.id = id;
      link.rel = "icon";
      link.href = href;
      document.head.appendChild(link);
    }

    stubMatchMedia(true);
    render(
      <AppThemeProvider>
        <div>content</div>
      </AppThemeProvider>,
    );

    expect(document.getElementById("favicon").getAttribute("href")).toBe(
      "/grain-mark-dark.svg",
    );
    expect(document.getElementById("favicon-png").getAttribute("href")).toBe(
      "/grain-mark-dark.png",
    );
  });
});
