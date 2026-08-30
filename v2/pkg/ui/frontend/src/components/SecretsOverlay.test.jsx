import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import SecretsOverlay from "./SecretsOverlay.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

describe("SecretsOverlay", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("shows a note instead of the list and form when not enabled", async () => {
    api.mockResolvedValueOnce({ enabled: false });
    render(<SecretsOverlay onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByText(/not available/i)).toBeInTheDocument();
    expect(screen.queryByLabelText("Secret")).not.toBeInTheDocument();
  });

  it("lists secrets and their keys when enabled", async () => {
    api.mockResolvedValueOnce({
      enabled: true,
      secrets: [{ name: "github", keys: ["token", "webhook-secret"] }],
    });
    render(<SecretsOverlay onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByText("github")).toBeInTheDocument();
    expect(screen.getByText("token")).toBeInTheDocument();
    expect(screen.getByText("webhook-secret")).toBeInTheDocument();
  });

  it("shows an empty message when enabled with no secrets", async () => {
    api.mockResolvedValueOnce({ enabled: true, secrets: [] });
    render(<SecretsOverlay onClose={() => {}} showError={() => {}} />);

    expect(await screen.findByText("No secrets set.")).toBeInTheDocument();
  });

  it("sets a secret via the form and refreshes", async () => {
    api
      .mockResolvedValueOnce({ enabled: true, secrets: [] })
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({ enabled: true, secrets: [{ name: "github", keys: ["token"] }] });
    const user = userEvent.setup();
    render(<SecretsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByText("No secrets set.");

    await user.type(screen.getByLabelText("Secret"), "github");
    await user.type(screen.getByLabelText("Key"), "token");
    await user.type(screen.getByLabelText(/Value/), "secret-value");
    await user.click(screen.getByRole("button", { name: "Set" }));

    expect(api).toHaveBeenCalledWith("/api/secrets/github/token", {
      method: "PUT",
      body: JSON.stringify({ value: "secret-value" }),
    });
    expect(await screen.findByText("github")).toBeInTheDocument();
  });

  it("deletes a single key", async () => {
    api
      .mockResolvedValueOnce({ enabled: true, secrets: [{ name: "github", keys: ["token"] }] })
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({ enabled: true, secrets: [] });
    const user = userEvent.setup();
    render(<SecretsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByText("github");

    await user.click(screen.getByTitle("delete github/token"));

    expect(api).toHaveBeenCalledWith("/api/secrets/github/token", { method: "DELETE" });
    expect(await screen.findByText("No secrets set.")).toBeInTheDocument();
  });

  it("deletes an entire secret", async () => {
    api
      .mockResolvedValueOnce({ enabled: true, secrets: [{ name: "github", keys: ["token"] }] })
      .mockResolvedValueOnce({})
      .mockResolvedValueOnce({ enabled: true, secrets: [] });
    const user = userEvent.setup();
    render(<SecretsOverlay onClose={() => {}} showError={() => {}} />);
    await screen.findByText("github");

    await user.click(screen.getByRole("button", { name: "Delete secret" }));

    expect(api).toHaveBeenCalledWith("/api/secrets/github", { method: "DELETE" });
    expect(await screen.findByText("No secrets set.")).toBeInTheDocument();
  });

  it("reports the error when setting a secret fails", async () => {
    api.mockResolvedValueOnce({ enabled: true, secrets: [] }).mockRejectedValueOnce(new Error("value is required"));
    const showError = vi.fn();
    const user = userEvent.setup();
    render(<SecretsOverlay onClose={() => {}} showError={showError} />);
    await screen.findByText("No secrets set.");

    await user.type(screen.getByLabelText("Secret"), "github");
    await user.type(screen.getByLabelText("Key"), "token");
    await user.type(screen.getByLabelText(/Value/), "x");
    await user.click(screen.getByRole("button", { name: "Set" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "value is required" }));
  });
});
