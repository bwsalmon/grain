import { afterEach, describe, expect, it, vi } from "vitest";
import api from "./api.js";

function jsonResponse(status, body) {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: "",
    headers: new Map([["Content-Type", "application/json"]]),
    json: () => Promise.resolve(body),
  };
}

describe("api", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("returns the parsed body on a JSON 2xx response", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(jsonResponse(200, { id: 7 })),
    );

    await expect(api("/api/tasks/7")).resolves.toEqual({ id: 7 });
  });

  it("sends JSON content-type by default and forwards opts through", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, null));
    vi.stubGlobal("fetch", fetchMock);

    await api("/api/tasks", { method: "POST", body: "{}" });

    expect(fetchMock).toHaveBeenCalledWith("/api/tasks", {
      method: "POST",
      body: "{}",
      headers: { "Content-Type": "application/json" },
    });
  });

  it("returns null for a non-JSON response body", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        status: 204,
        statusText: "No Content",
        headers: new Map(),
        json: () => Promise.reject(new Error("should not be called")),
      }),
    );

    await expect(
      api("/api/tasks/7/approve", { method: "POST" }),
    ).resolves.toBeNull();
  });

  it("throws the server's error message on a non-2xx JSON response", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValue(jsonResponse(400, { error: "title is required" })),
    );

    await expect(api("/api/tasks", { method: "POST" })).rejects.toThrow(
      "title is required",
    );
  });

  it("falls back to the status line when a non-2xx response carries no JSON error", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 500,
        statusText: "Internal Server Error",
        headers: new Map(),
        json: () => Promise.reject(new Error("should not be called")),
      }),
    );

    await expect(api("/api/tasks")).rejects.toThrow(
      "500 Internal Server Error",
    );
  });
});
