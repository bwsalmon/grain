import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import RepoField from "./RepoField.jsx";

describe("RepoField", () => {
  it("renders a plain text input when there are no known repos", () => {
    render(<RepoField name="repo" options={[]} />);
    const field = screen.getByRole("textbox");
    expect(field.tagName).toBe("INPUT");
  });

  it("renders a dropdown of the known repos, plus a blank option when not required", () => {
    render(<RepoField name="repo" options={["acme/widgets", "acme/other"]} />);
    const field = screen.getByRole("combobox");
    expect(field.tagName).toBe("SELECT");
    expect(screen.getByRole("option", { name: "acme/widgets" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "acme/other" })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "—" })).toBeInTheDocument();
  });

  it("omits the blank option when required", () => {
    render(<RepoField name="repo" options={["acme/widgets"]} required />);
    expect(screen.queryByRole("option", { name: "—" })).not.toBeInTheDocument();
  });

  it("starts as a dropdown pre-selecting a default value that is one of the options", () => {
    render(<RepoField name="repo" options={["acme/widgets", "acme/other"]} defaultValue="acme/other" />);
    expect(screen.getByRole("combobox")).toHaveValue("acme/other");
  });

  it("starts as a text input when the default value is not one of the options", () => {
    render(<RepoField name="repo" options={["acme/widgets"]} defaultValue="acme/unlisted" />);
    expect(screen.getByRole("textbox")).toHaveValue("acme/unlisted");
  });

  it("switches to a free-text input after picking Other…, and back after Choose from list", async () => {
    const user = userEvent.setup();
    render(<RepoField name="repo" options={["acme/widgets"]} />);

    await user.selectOptions(screen.getByRole("combobox"), "Other…");
    const field = screen.getByRole("textbox");
    await user.type(field, "acme/new-repo");
    expect(field).toHaveValue("acme/new-repo");

    await user.click(screen.getByRole("button", { name: "Choose from list" }));
    expect(screen.getByRole("combobox")).toBeInTheDocument();
  });
});
