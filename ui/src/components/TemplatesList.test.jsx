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
  autoMerge: false,
  reads: [],
  capabilities: [],
  createdAt: "2026-01-01T00:00:00Z",
};

const otherTemplate = {
  id: "template-2",
  name: "Security patch",
  title: "Apply security patches",
  description: "",
  autoMerge: false,
  reads: [],
  capabilities: [],
  createdAt: "2026-02-01T00:00:00Z",
};

const noop = () => {};

describe("TemplatesList", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("lists the templates it is given, showing just their key details", () => {
    render(<TemplatesList templates={[template]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("Dependency bump")).toBeInTheDocument();
    expect(screen.getByText("Bump dependencies")).toBeInTheDocument();
    // No form fields on the main list any more -- editing lives behind
    // clicking a row instead.
    expect(screen.queryByLabelText(/Name/)).not.toBeInTheDocument();
  });

  it("shows an empty message when there are none", () => {
    render(<TemplatesList templates={[]} onRefresh={noop} showError={noop} />);

    expect(screen.getByText("No task templates.")).toBeInTheDocument();
    // Nothing to search or sort when the list is empty.
    expect(screen.queryByPlaceholderText("Search templates…")).not.toBeInTheDocument();
  });

  it("filters the list by name or title", async () => {
    const user = userEvent.setup();
    render(<TemplatesList templates={[template, otherTemplate]} onRefresh={noop} showError={noop} />);

    await user.type(screen.getByPlaceholderText("Search templates…"), "security");

    expect(screen.getByText("Security patch")).toBeInTheDocument();
    expect(screen.queryByText("Dependency bump")).not.toBeInTheDocument();
  });

  it("shows a message when a search matches nothing", async () => {
    const user = userEvent.setup();
    render(<TemplatesList templates={[template]} onRefresh={noop} showError={noop} />);

    await user.type(screen.getByPlaceholderText("Search templates…"), "nope");

    expect(screen.getByText("No templates match your search.")).toBeInTheDocument();
  });

  it("opens a blank overlay from the + button and submits a new template", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<TemplatesList templates={[]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByRole("button", { name: "+ New template" }));
    expect(screen.getByRole("heading", { name: "New template" })).toBeInTheDocument();

    await user.type(screen.getByLabelText(/Name/), "Dependency bump");
    await user.type(screen.getByLabelText(/Task title/), "Bump dependencies");
    await user.click(screen.getByRole("button", { name: "Add template" }));

    expect(api).toHaveBeenCalledWith("/api/templates", {
      method: "POST",
      body: JSON.stringify({
        name: "Dependency bump",
        title: "Bump dependencies",
        description: "",
        autoMerge: false,
        reads: [],
        capabilities: [],
      }),
    });
    expect(onRefresh).toHaveBeenCalled();
    expect(screen.queryByRole("heading", { name: "New template" })).not.toBeInTheDocument();
  });

  it("opens a row's overlay pre-filled and saves changes via PATCH", async () => {
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<TemplatesList templates={[template]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByText("Dependency bump"));
    expect(screen.getByRole("heading", { name: "Edit template" })).toBeInTheDocument();

    const nameField = screen.getByLabelText(/Name/);
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
        autoMerge: false,
        reads: [],
        capabilities: [],
      }),
    });
    expect(onRefresh).toHaveBeenCalled();
    expect(screen.queryByRole("heading", { name: "Edit template" })).not.toBeInTheDocument();
  });

  it("deletes a template from its overlay after confirming", async () => {
    vi.stubGlobal("confirm", vi.fn().mockReturnValue(true));
    api.mockResolvedValueOnce({});
    const onRefresh = vi.fn();
    const user = userEvent.setup();
    render(<TemplatesList templates={[template]} onRefresh={onRefresh} showError={noop} />);

    await user.click(screen.getByText("Dependency bump"));
    await user.click(screen.getByRole("button", { name: "Delete" }));

    expect(api).toHaveBeenCalledWith("/api/templates/template-1", { method: "DELETE" });
    expect(onRefresh).toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("cancels an edit without saving", async () => {
    const user = userEvent.setup();
    render(<TemplatesList templates={[template]} onRefresh={noop} showError={noop} />);

    await user.click(screen.getByText("Dependency bump"));
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    expect(api).not.toHaveBeenCalled();
  });

  it("reports the error and leaves the overlay open when creation fails", async () => {
    api.mockRejectedValueOnce(new Error("unknown capability not-a-real-capability"));
    const showError = vi.fn();
    const user = userEvent.setup();
    render(<TemplatesList templates={[]} onRefresh={noop} showError={showError} />);

    await user.click(screen.getByRole("button", { name: "+ New template" }));
    await user.type(screen.getByLabelText(/Name/), "x");
    await user.type(screen.getByLabelText(/Task title/), "x");
    await user.click(screen.getByRole("button", { name: "Add template" }));

    expect(showError).toHaveBeenCalledWith(expect.objectContaining({ message: "unknown capability not-a-real-capability" }));
    expect(screen.getByRole("heading", { name: "New template" })).toBeInTheDocument();
  });
});
