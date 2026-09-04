import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
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
    expect(
      screen.getByRole("option", { name: "acme/widgets" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("option", { name: "acme/other" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "—" })).toBeInTheDocument();
  });

  // grain/task-320: the repos figure goes in front of the field rather
  // than on each option, because an <option> can hold no markup and this
  // stays a native <select> -- but it is the same ring the Repos page,
  // the nav rail and the read-only repos picker carry (ItemGlyph.jsx,
  // docs/brand.md), in both of this field's two states.
  it("marks the field with the repos glyph, as a dropdown and as free text", async () => {
    const user = userEvent.setup();
    const { container } = render(
      <RepoField name="repo" options={["acme/widgets"]} />,
    );

    expect(
      container.querySelector('.repo-field svg[data-glyph="repos"]'),
    ).toBeInTheDocument();

    await user.selectOptions(screen.getByRole("combobox"), "Other…");
    expect(
      container.querySelector('.repo-field svg[data-glyph="repos"]'),
    ).toBeInTheDocument();
  });

  it("omits the blank option when required", () => {
    render(<RepoField name="repo" options={["acme/widgets"]} required />);
    expect(screen.queryByRole("option", { name: "—" })).not.toBeInTheDocument();
  });

  it("starts as a dropdown pre-selecting a default value that is one of the options", () => {
    render(
      <RepoField
        name="repo"
        options={["acme/widgets", "acme/other"]}
        defaultValue="acme/other"
      />,
    );
    expect(screen.getByRole("combobox")).toHaveValue("acme/other");
  });

  it("starts as a text input when the default value is not one of the options", () => {
    render(
      <RepoField
        name="repo"
        options={["acme/widgets"]}
        defaultValue="acme/unlisted"
      />,
    );
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

  it("reports the picked value to onChange as the dropdown selection changes", async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    render(
      <RepoField
        name="repo"
        options={["acme/widgets", "acme/other"]}
        onChange={onChange}
      />,
    );

    await user.selectOptions(screen.getByRole("combobox"), "acme/other");
    expect(onChange).toHaveBeenCalledWith("acme/other");
  });

  it("reports the picked value to onChange while typing in the free-text input", async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    render(<RepoField name="repo" options={[]} onChange={onChange} />);

    await user.type(screen.getByRole("textbox"), "a");
    expect(onChange).toHaveBeenCalledWith("a");
  });
});
