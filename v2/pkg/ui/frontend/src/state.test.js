import { describe, expect, it } from "vitest";
import { capabilityName } from "./state.js";

describe("capabilityName", () => {
  const config = { capabilities: [{ id: "web-search", name: "Web search" }] };

  it("returns the matching capability's name", () => {
    expect(capabilityName(config, "web-search")).toBe("Web search");
  });

  it("falls back to the id when no capability matches", () => {
    expect(capabilityName(config, "unknown")).toBe("unknown");
  });

  it("falls back to the id when config is null", () => {
    expect(capabilityName(null, "web-search")).toBe("web-search");
  });

  it("falls back to the id when config has no capabilities", () => {
    expect(capabilityName({}, "web-search")).toBe("web-search");
  });
});
