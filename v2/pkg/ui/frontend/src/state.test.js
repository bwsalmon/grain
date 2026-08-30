import { describe, expect, it } from "vitest";
import { capabilityName, knownRepos } from "./state.js";

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

describe("knownRepos", () => {
  it("unions targetRepos and repos already seen on tasks, sorted and deduped", () => {
    const config = { targetRepos: ["acme/widgets", "acme/other"] };
    const tasks = [{ repo: "acme/other" }, { repo: "acme/newer" }, { repo: "" }];
    expect(knownRepos(config, tasks)).toEqual(["acme/newer", "acme/other", "acme/widgets"]);
  });

  it("returns an empty list when nothing is configured or targeted yet", () => {
    expect(knownRepos(null, [])).toEqual([]);
    expect(knownRepos(null, null)).toEqual([]);
  });

  it("falls back to tasks alone on an unrestricted deployment", () => {
    expect(knownRepos({ targetRepos: [] }, [{ repo: "acme/widgets" }])).toEqual(["acme/widgets"]);
  });
});
