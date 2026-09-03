import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import AgentKeysSection from "./AgentKeysSection.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const enabled = {
  agentKeysEnabled: true,
  geminiApiKeySet: false,
  claudeOAuthTokenSet: false,
  openaiApiKeySet: false,
};

describe("AgentKeysSection", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("says so, and offers nothing to type into, when there is no secrets store", () => {
    render(<AgentKeysSection settings={{ agentKeysEnabled: false }} showError={() => {}} />);

    expect(screen.getByText(/cannot be set from here/i)).toBeInTheDocument();
    expect(screen.queryByLabelText("Claude Code OAuth token")).not.toBeInTheDocument();
  });

  it("reports which keys are already set, without showing a value", () => {
    render(
      <AgentKeysSection settings={{ ...enabled, claudeOAuthTokenSet: true }} showError={() => {}} />,
    );

    expect(screen.getByText("Gemini API key")).toBeInTheDocument();
    expect(screen.getByText("Claude Code OAuth token")).toBeInTheDocument();
    expect(screen.getByText("OpenAI API key")).toBeInTheDocument();
    expect(screen.getByText("set")).toBeInTheDocument();
    // One chip per framework whose key is missing: two of the three here.
    expect(screen.getAllByText("not set")).toHaveLength(2);
    expect(screen.getByLabelText("Claude Code OAuth token")).toHaveValue("");
  });

  it("stores a pasted token and flips its chip to set", async () => {
    api.mockResolvedValueOnce({ ...enabled, claudeOAuthTokenSet: true });
    const user = userEvent.setup();
    render(<AgentKeysSection settings={enabled} showError={() => {}} />);

    await user.type(screen.getByLabelText("Claude Code OAuth token"), "sk-ant-oat01-fake");
    await user.click(screen.getAllByRole("button", { name: "Set" })[1]);

    expect(api).toHaveBeenCalledWith("/api/agent-keys/claude", {
      method: "PUT",
      body: JSON.stringify({ value: "sk-ant-oat01-fake" }),
    });
    expect(await screen.findByText("set")).toBeInTheDocument();
    // Cleared from the field once stored: it is write-only, so leaving
    // it on screen would be the only place in the UI a credential is
    // readable.
    expect(screen.getByLabelText("Claude Code OAuth token")).toHaveValue("");
  });

  it("clears a stored key", async () => {
    api.mockResolvedValueOnce({ ...enabled, geminiApiKeySet: false });
    const user = userEvent.setup();
    render(<AgentKeysSection settings={{ ...enabled, geminiApiKeySet: true }} showError={() => {}} />);

    await user.click(screen.getAllByRole("button", { name: "Clear" })[0]);

    expect(api).toHaveBeenCalledWith("/api/agent-keys/antigravity", { method: "DELETE" });
    expect(await screen.findAllByText("not set")).toHaveLength(3);
  });

  it("stores an OpenAI key under the codex framework's own endpoint", async () => {
    api.mockResolvedValueOnce({ ...enabled, openaiApiKeySet: true });
    const user = userEvent.setup();
    render(<AgentKeysSection settings={enabled} showError={() => {}} />);

    await user.type(screen.getByLabelText("OpenAI API key"), "sk-openai-fake");
    await user.click(screen.getAllByRole("button", { name: "Set" })[2]);

    expect(api).toHaveBeenCalledWith("/api/agent-keys/codex", {
      method: "PUT",
      body: JSON.stringify({ value: "sk-openai-fake" }),
    });
    expect(await screen.findByText("set")).toBeInTheDocument();
    expect(screen.getByLabelText("OpenAI API key")).toHaveValue("");
  });

  it("cannot set an empty value, or clear a key that is not set", () => {
    render(<AgentKeysSection settings={enabled} showError={() => {}} />);

    expect(screen.getAllByRole("button", { name: "Set" })[0]).toBeDisabled();
    expect(screen.getAllByRole("button", { name: "Clear" })[0]).toBeDisabled();
  });
});
