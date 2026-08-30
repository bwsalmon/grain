import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import TemplatesList from "./TemplatesList.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const template = {
  id: "template-1",
  name: "Dependency bump",
  title: "Bump dependencies",
  description: "",
  repo: "acme/widgets",
  base: "",
  autoMerge: false,
  reads: [],
  capabilities: [],
};

const noop = () => {};

describe("TemplatesList", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("lists the templates it is given", () => {
    render(<TemplatesList templates={[template]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("Dependency bump")).toBeInTheDocument();
    expect(screen.getByText("acme/widgets")).toBeInTheDocument();
    expect(screen.getByText("Bump dependencies")).toBeInTheDocument();
  });

  it("shows an empty message when there are none", () => {
    render(<TemplatesList templates={[]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("No task templates.")).toBeInTheDocument();
  });

  it("submits a new template with the expected fields", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<TemplatesList templates={[]} onRefresh={onRefresh} showError={noop} />);

    await user.type(screen.getByLabelText(/Name/), "Dependency bump");
    await user.type(screen.getByLabelText(/Task title/), "Bump dependencies");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Add template" }));

    expect(api).toHaveBeenCalledWith("/api/templates", {
      method: "POST",
      body: JSON.stringify({
        name: "Dependency bump",
        title: "Bump dependencies",
        description: "",
        repo: "acme/widgets",
        base: "",
        autoMerge: false,
        reads: [],
        capabilities: [],
      }),
    });
    expect(onRefresh).toHaveBeenCalled();
  });

  it("deletes a template after confirming", async () => {
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<TemplatesList templates={[template]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "Delete" }));

    expect(api).toHaveBeenCalledWith("/api/templates/template-1", { method: "DELETE" });
    expect(onRefresh).toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("opens an edit form pre-filled with the template's fields and saves changes via PATCH", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<TemplatesList templates={[template]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "Edit" }));

    const nameField = screen.getAllByLabelText(/Name/)[0];
    expect(nameField).toHaveValue("Dependency bump");

    await user.clear(nameField);
    await user.type(nameField, "Dependency bump (patch only)");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(api).toHaveBeenCalledWith("/api/templates/template-1", {
      method: "PATCH",
      body: JSON.stringify({
        name: "Dependency bump (patch only)",
        title: "Bump dependencies",
        description: "",
        repo: "acme/widgets",
        base: "",
        autoMerge: false,
        reads: [],
        capabilities: [],
      }),
    });
    expect(onRefresh).toHaveBeenCalled();
  });

  it("cancels an edit without saving", async () => {
    const user = userEvent.setup();
    render(<TemplatesList templates={[template]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "Edit" }));
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Edit" })).toBeInTheDocument();
    expect(api).not.toHaveBeenCalled();
  });

  it("reports the error and leaves the form open when creation fails", async () => {
    api.mockRejectedValueOnce(new Error("unknown capability not-a-real-capability"));
    const showError = vi.fn();
    const user = userEvent.setup();
    render(<TemplatesList templates={[]} onRefresh={noop} showError={showError} />);

    await user.type(screen.getByLabelText(/Name/), "x");
    await user.type(screen.getByLabelText(/Task title/), "x");
    await user.type(screen.getByLabelText(/Target repo/), "acme/widgets");
    await user.click(screen.getByRole("button", { name: "Add template" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "unknown capability not-a-real-capability" }));
  });
});
