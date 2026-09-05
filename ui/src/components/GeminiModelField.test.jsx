import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import GeminiModelField from "./GeminiModelField.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const catalog = {
  available: true,
  models: [
    {
      id: "gemini-3.1-pro-high",
      label: "Gemini 3.1 Pro (High)",
      effort: "high",
      family: "gemini-3.1-pro",
    },
    {
      id: "gemini-3.1-pro-low",
      label: "Gemini 3.1 Pro (Low)",
      effort: "low",
      family: "gemini-3.1-pro",
    },
  ],
  efforts: ["low", "medium", "high"],
};

describe("GeminiModelField", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("offers what agy lists, and names the effort vocabulary", async () => {
    api.mockResolvedValueOnce(catalog);
    const user = userEvent.setup();
    render(<GeminiModelField defaultValue="gemini-3.1-pro-high" />);

    expect(
      await screen.findByText(/what `agy models` lists on this host/),
    ).toHaveTextContent("low, medium, high");

    await user.click(screen.getByLabelText(/Open/));
    expect(await screen.findByText("gemini-3.1-pro-low")).toBeInTheDocument();
    // agy's own display name, beside the id that actually goes in the
    // setting.
    expect(screen.getByText("Gemini 3.1 Pro (Low)")).toBeInTheDocument();
  });

  it("selects a listed model into the field the form reads", async () => {
    api.mockResolvedValueOnce(catalog);
    const user = userEvent.setup();
    render(<GeminiModelField defaultValue="gemini-3.1-pro-high" />);
    await screen.findByText(/what `agy models` lists on this host/);

    await user.click(screen.getByLabelText(/Open/));
    await user.click(await screen.findByText("gemini-3.1-pro-low"));

    const input = document.querySelector("input[name=geminiModel]");
    expect(input).toHaveValue("gemini-3.1-pro-low");
  });

  // The whole reason it is a write-in and not a Select: a name the
  // catalog does not carry still saves, and the field says what that
  // means rather than refusing it.
  it("takes a written-in model the catalog does not list", async () => {
    api.mockResolvedValueOnce(catalog);
    const user = userEvent.setup();
    render(<GeminiModelField defaultValue="" />);
    await screen.findByText(/what `agy models` lists on this host/);

    const input = document.querySelector("input[name=geminiModel]");
    await user.type(input, "gemini-9-pro-high");

    expect(input).toHaveValue("gemini-9-pro-high");
    expect(screen.getByText(/not in agy's own catalog/)).toBeInTheDocument();
  });

  it("stays typable, and says why, when the catalog cannot be read", async () => {
    api.mockResolvedValueOnce({
      available: true,
      error: "the Antigravity CLI (agy) is not installed",
    });
    const user = userEvent.setup();
    render(<GeminiModelField defaultValue="" />);

    expect(
      await screen.findByText(/could not read agy's model catalog/),
    ).toHaveTextContent("not installed");
    const input = document.querySelector("input[name=geminiModel]");
    await user.type(input, "gemini-3.1-pro-high");
    expect(input).toHaveValue("gemini-3.1-pro-high");
  });

  it("stays typable when this deployment has nothing to ask", async () => {
    api.mockResolvedValueOnce({ available: false });
    render(<GeminiModelField defaultValue="gemini-3.1-pro-high" />);

    expect(
      await screen.findByText(/cannot read agy's model catalog/),
    ).toBeInTheDocument();
    expect(document.querySelector("input[name=geminiModel]")).toHaveValue(
      "gemini-3.1-pro-high",
    );
  });

  it("stays typable when the request itself fails", async () => {
    api.mockRejectedValueOnce(new Error("503 Service Unavailable"));
    render(<GeminiModelField defaultValue="gemini-3.1-pro-high" />);

    expect(
      await screen.findByText(/could not read agy's model catalog/),
    ).toHaveTextContent("503");
    expect(document.querySelector("input[name=geminiModel]")).toHaveValue(
      "gemini-3.1-pro-high",
    );
  });
});
