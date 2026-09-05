import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import GeminiModelFields from "./GeminiModelFields.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const catalog = {
  available: true,
  models: [
    {
      id: "gemini-3.8-flash-medium",
      label: "Gemini 3.8 Flash (Medium)",
      effort: "medium",
      family: "gemini-3.8-flash",
    },
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

// The vocabulary the server validates a save against, as the Settings
// response reports it (Settings.geminiEfforts).
const vocabulary = ["low", "medium", "high"];

const renderFields = (props) =>
  render(
    <GeminiModelFields
      defaultModel=""
      defaultEffort=""
      efforts={vocabulary}
      {...props}
    />,
  );

describe("GeminiModelFields", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("offers what agy lists, with agy's own label beside each id", async () => {
    api.mockResolvedValueOnce(catalog);
    const user = userEvent.setup();
    renderFields({ defaultModel: "gemini-3.1-pro-high" });
    await screen.findByText(/what `agy models` lists on this host/);

    await user.click(screen.getByLabelText(/Open/));
    expect(await screen.findByText("gemini-3.1-pro-low")).toBeInTheDocument();
    expect(screen.getByText("Gemini 3.1 Pro (Low)")).toBeInTheDocument();
  });

  it("selects a listed model into the field the form reads", async () => {
    api.mockResolvedValueOnce(catalog);
    const user = userEvent.setup();
    renderFields({ defaultModel: "gemini-3.1-pro-high" });
    await screen.findByText(/what `agy models` lists on this host/);

    await user.click(screen.getByLabelText(/Open/));
    await user.click(await screen.findByText("gemini-3.1-pro-low"));

    expect(document.querySelector("input[name=geminiModel]")).toHaveValue(
      "gemini-3.1-pro-low",
    );
  });

  // The whole reason it is a write-in and not a Select: a name the
  // catalog does not carry still saves, and the field says what that
  // means rather than refusing it.
  it("takes a written-in model the catalog does not list", async () => {
    api.mockResolvedValueOnce(catalog);
    const user = userEvent.setup();
    renderFields();
    await screen.findByText(/what `agy models` lists on this host/);

    const input = document.querySelector("input[name=geminiModel]");
    await user.type(input, "gemini-9-pro-high");

    expect(input).toHaveValue("gemini-9-pro-high");
    expect(screen.getByText(/not in agy's own catalog/)).toBeInTheDocument();
  });

  // Which efforts a model has is the model's own business, and the
  // catalog is where it is written down: 3.1 Pro is listed high and low,
  // and agy refuses medium for it before the run starts.
  it("offers only the efforts the chosen model was listed with", async () => {
    api.mockResolvedValueOnce(catalog);
    const user = userEvent.setup();
    renderFields({
      defaultModel: "gemini-3.1-pro-high",
      defaultEffort: "high",
    });
    await screen.findByText(/what `agy models` lists on this host/);

    await user.click(screen.getByLabelText(/Gemini reasoning effort/));
    expect(screen.getByRole("option", { name: "low" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "high" })).toBeInTheDocument();
    expect(
      screen.queryByRole("option", { name: "medium" }),
    ).not.toBeInTheDocument();
  });

  it("offers the server's whole vocabulary for a model it cannot place", async () => {
    api.mockResolvedValueOnce(catalog);
    const user = userEvent.setup();
    renderFields({ defaultModel: "gemini-9-pro" });
    await screen.findByText(/not in agy's own catalog/);

    await user.click(screen.getByLabelText(/Gemini reasoning effort/));
    for (const effort of vocabulary) {
      expect(screen.getByRole("option", { name: effort })).toBeInTheDocument();
    }
  });

  it("stays typable, and says why, when the catalog cannot be read", async () => {
    api.mockResolvedValueOnce({
      available: true,
      error: "the Antigravity CLI (agy) is not installed",
    });
    const user = userEvent.setup();
    renderFields();

    expect(
      await screen.findByText(/could not read agy's model catalog/),
    ).toHaveTextContent("not installed");
    const input = document.querySelector("input[name=geminiModel]");
    await user.type(input, "gemini-3.1-pro-high");
    expect(input).toHaveValue("gemini-3.1-pro-high");
    // And the effort is still settable, from the vocabulary the server
    // named: a catalog nobody could read must not cost either setting.
    await user.click(screen.getByLabelText(/Gemini reasoning effort/));
    expect(screen.getByRole("option", { name: "medium" })).toBeInTheDocument();
  });

  it("stays typable when this deployment has nothing to ask", async () => {
    api.mockResolvedValueOnce({ available: false });
    renderFields({ defaultModel: "gemini-3.1-pro-high" });

    expect(
      await screen.findByText(/cannot read agy's model catalog/),
    ).toBeInTheDocument();
    expect(document.querySelector("input[name=geminiModel]")).toHaveValue(
      "gemini-3.1-pro-high",
    );
  });

  it("stays typable when the request itself fails", async () => {
    api.mockRejectedValueOnce(new Error("503 Service Unavailable"));
    renderFields({ defaultModel: "gemini-3.1-pro-high" });

    expect(
      await screen.findByText(/could not read agy's model catalog/),
    ).toHaveTextContent("503");
    expect(document.querySelector("input[name=geminiModel]")).toHaveValue(
      "gemini-3.1-pro-high",
    );
  });
});
