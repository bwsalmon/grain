import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import SecretField, { SecretFields } from "./SecretField.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

const minter = {
  name: "gcp-key-minter",
  secret: "gcp-key-minter",
  key: "value",
  set: false,
};

describe("SecretField", () => {
  afterEach(() => {
    api.mockReset();
  });

  it("writes the value to the secret and key the API named, then asks for a refresh", async () => {
    api.mockResolvedValueOnce({});
    const onChanged = vi.fn();
    const user = userEvent.setup();
    render(
      <SecretField
        secret={minter}
        showError={() => {}}
        onChanged={onChanged}
      />,
    );

    await user.type(screen.getByLabelText("gcp-key-minter"), "a-key");
    await user.click(screen.getByRole("button", { name: "Set" }));

    expect(api).toHaveBeenCalledWith("/api/secrets/gcp-key-minter/value", {
      method: "PUT",
      body: JSON.stringify({ value: "a-key" }),
    });
    expect(onChanged).toHaveBeenCalled();
  });

  // The value is write-only, so nothing comes back to show: the box is
  // emptied and the chip the reloaded settings bring is what says it
  // landed.
  it("clears the box after a successful set", async () => {
    api.mockResolvedValueOnce({});
    const user = userEvent.setup();
    render(
      <SecretField secret={minter} showError={() => {}} onChanged={() => {}} />,
    );

    const box = screen.getByLabelText("gcp-key-minter");
    await user.type(box, "a-key");
    await user.click(screen.getByRole("button", { name: "Set" }));

    expect(box).toHaveValue("");
  });

  it("cannot be set empty, and cannot be cleared when nothing is set", () => {
    render(
      <SecretField secret={minter} showError={() => {}} onChanged={() => {}} />,
    );

    expect(screen.getByRole("button", { name: "Set" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Clear" })).toBeDisabled();
    expect(screen.getByText("not set")).toBeInTheDocument();
  });

  it("deletes just the one key when cleared", async () => {
    api.mockResolvedValueOnce({});
    const onChanged = vi.fn();
    const user = userEvent.setup();
    render(
      <SecretField
        secret={{
          name: "github-app/app-id",
          secret: "github-app",
          key: "app-id",
          set: true,
        }}
        showError={() => {}}
        onChanged={onChanged}
      />,
    );

    expect(screen.getByText("set")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Clear" }));

    expect(api).toHaveBeenCalledWith("/api/secrets/github-app/app-id", {
      method: "DELETE",
    });
    expect(onChanged).toHaveBeenCalled();
  });

  it("reports a failed write instead of pretending it landed", async () => {
    api.mockRejectedValueOnce(new Error("value is required"));
    const showError = vi.fn();
    const onChanged = vi.fn();
    const user = userEvent.setup();
    render(
      <SecretField
        secret={minter}
        showError={showError}
        onChanged={onChanged}
      />,
    );

    await user.type(screen.getByLabelText("gcp-key-minter"), "x");
    await user.click(screen.getByRole("button", { name: "Set" }));

    expect(showError).toHaveBeenCalledWith(
      expect.objectContaining({ message: "value is required" }),
    );
    expect(onChanged).not.toHaveBeenCalled();
  });

  describe("SecretFields", () => {
    it("renders nothing for a capability that resolves no credential", () => {
      const { container } = render(
        <SecretFields secrets={[]} showError={() => {}} onChanged={() => {}} />,
      );
      expect(container).toBeEmptyDOMElement();
    });

    it("renders one field per credential", () => {
      render(
        <SecretFields
          secrets={[
            {
              name: "github-app/app-id",
              secret: "github-app",
              key: "app-id",
              set: true,
            },
            {
              name: "github-app/private-key",
              secret: "github-app",
              key: "private-key",
              set: false,
            },
          ]}
          showError={() => {}}
          onChanged={() => {}}
        />,
      );

      expect(screen.getByText("Credentials this needs:")).toBeInTheDocument();
      expect(screen.getByLabelText("github-app/app-id")).toBeInTheDocument();
      expect(
        screen.getByLabelText("github-app/private-key"),
      ).toBeInTheDocument();
    });
  });
});
