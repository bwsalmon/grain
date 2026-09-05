import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import RootShellPage from "./RootShellPage.jsx";
import api from "../api.js";

vi.mock("../api.js", () => ({ default: vi.fn() }));

// grain/task-13: the System pane's last resort -- one command as root on
// the machine the daemon runs on, for the failure none of the panels
// beside it could explain.
describe("RootShellPage", () => {
  const enabled = { rootShellEnabled: true };

  afterEach(() => {
    api.mockReset();
  });

  it("says so, and offers no prompt, where the deployment has no root shell", () => {
    render(
      <RootShellPage
        config={{ rootShellEnabled: false }}
        showError={() => {}}
      />,
    );

    expect(screen.getByText(/not available/i)).toBeInTheDocument();
    expect(
      screen.queryByRole("textbox", { name: /command/i }),
    ).not.toBeInTheDocument();
    expect(api).not.toHaveBeenCalled();
  });

  // The warning is standing rather than a confirm on each command: a
  // per-command confirm is clicked through by the second one, and this
  // pane is used a dozen short commands at a time.
  it("warns that every command runs as root on the host", () => {
    render(<RootShellPage config={enabled} showError={() => {}} />);

    expect(screen.getByText(/runs as/i)).toBeInTheDocument();
    expect(screen.getByText("root")).toBeInTheDocument();
  });

  it("runs a command and shows what it printed", async () => {
    api.mockResolvedValueOnce({
      output: "uid=0(root) gid=0(root)\n",
      exitCode: 0,
    });
    const user = userEvent.setup();
    render(<RootShellPage config={enabled} showError={() => {}} />);

    await user.type(screen.getByRole("textbox", { name: /command/i }), "id");
    await user.click(screen.getByRole("button", { name: /run as root/i }));

    expect(api).toHaveBeenCalledWith("/api/host/shell", {
      method: "POST",
      body: JSON.stringify({ command: "id" }),
    });
    expect(await screen.findByText(/uid=0\(root\)/)).toBeInTheDocument();
    // The command itself is in the scrollback too: the output of the
    // third command in a row means nothing without it.
    expect(screen.getByText(/# id/)).toBeInTheDocument();
  });

  // A command that failed is an answer, not an error banner: it comes
  // back as a 200 carrying its exit code, and the pane shows both.
  it("shows a failing command's output and its exit code", async () => {
    api.mockResolvedValueOnce({
      output: "Unit nope.service not found\n",
      exitCode: 4,
    });
    const user = userEvent.setup();
    const showError = vi.fn();
    render(<RootShellPage config={enabled} showError={showError} />);

    await user.type(
      screen.getByRole("textbox", { name: /command/i }),
      "systemctl status nope",
    );
    await user.click(screen.getByRole("button", { name: /run as root/i }));

    expect(await screen.findByText(/not found/)).toBeInTheDocument();
    expect(screen.getByText(/\[exit 4\]/)).toBeInTheDocument();
    expect(showError).not.toHaveBeenCalled();
  });

  // The exchange failing is a different thing -- no responder installed
  // on the host -- and it goes to the error banner, with the command
  // left in the box to run again once that is fixed.
  it("reports a failure of the exchange itself and keeps the command", async () => {
    api.mockRejectedValueOnce(
      new Error("no answer from this host's root shell responder"),
    );
    const user = userEvent.setup();
    const showError = vi.fn();
    render(<RootShellPage config={enabled} showError={showError} />);

    await user.type(screen.getByRole("textbox", { name: /command/i }), "id");
    await user.click(screen.getByRole("button", { name: /run as root/i }));

    expect(showError).toHaveBeenCalledWith(
      new Error("no answer from this host's root shell responder"),
    );
    expect(screen.getByRole("textbox", { name: /command/i })).toHaveValue("id");
  });

  it("will not send an empty command", async () => {
    render(<RootShellPage config={enabled} showError={() => {}} />);

    expect(screen.getByRole("button", { name: /run as root/i })).toBeDisabled();
    expect(api).not.toHaveBeenCalled();
  });

  it("keeps a scrollback of everything run in this session", async () => {
    api
      .mockResolvedValueOnce({ output: "first\n", exitCode: 0 })
      .mockResolvedValueOnce({ output: "second\n", exitCode: 0 });
    const user = userEvent.setup();
    render(<RootShellPage config={enabled} showError={() => {}} />);
    const box = screen.getByRole("textbox", { name: /command/i });

    await user.type(box, "one");
    await user.click(screen.getByRole("button", { name: /run as root/i }));
    await screen.findByText(/first/);
    await user.type(box, "two");
    await user.click(screen.getByRole("button", { name: /run as root/i }));

    expect(await screen.findByText(/second/)).toBeInTheDocument();
    expect(screen.getByText(/first/)).toBeInTheDocument();
  });

  // Enter runs it, the way a prompt does: the form is what makes the
  // pane usable without reaching for the mouse between commands.
  it("runs the command on Enter", async () => {
    api.mockResolvedValueOnce({ output: "ok\n", exitCode: 0 });
    const user = userEvent.setup();
    render(<RootShellPage config={enabled} showError={() => {}} />);

    await user.type(
      screen.getByRole("textbox", { name: /command/i }),
      "uptime{Enter}",
    );

    expect(api).toHaveBeenCalledWith("/api/host/shell", {
      method: "POST",
      body: JSON.stringify({ command: "uptime" }),
    });
    expect(await screen.findByText(/ok/)).toBeInTheDocument();
  });
});
