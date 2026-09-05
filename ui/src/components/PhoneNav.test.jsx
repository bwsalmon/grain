import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import PhoneNav from "./PhoneNav.jsx";

// The rail stands in for the real Sidebar: what PhoneNav does with it is
// the same whatever it holds -- it is handed one element, keeps it out of
// the way until the menu is tapped, and puts it away again on the way out.
function Rail({ onNavigate = () => {} }) {
  return (
    <div>
      <button onClick={onNavigate}>Board</button>
    </div>
  );
}

describe("PhoneNav", () => {
  it("keeps the rail off the screen until the menu is tapped", async () => {
    render(
      <PhoneNav config={{}} running={false} onOpenNewTask={() => {}}>
        <Rail />
      </PhoneNav>,
    );

    // The bar is what is on screen; the rail's own entries are not.
    expect(screen.getByRole("heading", { name: "grain" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Board" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Open navigation" }));

    expect(
      await screen.findByRole("button", { name: "Board" }),
    ).toBeInTheDocument();
  });

  // Every control in the rail is a navigation, so the drawer closes on
  // any tap that lands inside it rather than staying open over the page
  // it just navigated to.
  it("closes the drawer when a nav entry inside it is tapped", async () => {
    const onNavigate = vi.fn();
    render(
      <PhoneNav config={{}} running={false} onOpenNewTask={() => {}}>
        <Rail onNavigate={onNavigate} />
      </PhoneNav>,
    );

    fireEvent.click(screen.getByRole("button", { name: "Open navigation" }));
    fireEvent.click(await screen.findByRole("button", { name: "Board" }));

    // The entry's own handler still ran: closing the drawer is in
    // addition to navigating, not instead of it.
    expect(onNavigate).toHaveBeenCalled();
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Board" })).toBeNull(),
    );
  });

  it("files a task straight from the bar, without opening the drawer", () => {
    const onOpenNewTask = vi.fn();
    render(
      <PhoneNav config={{}} running={false} onOpenNewTask={onOpenNewTask}>
        <Rail />
      </PhoneNav>,
    );

    fireEvent.click(screen.getByRole("button", { name: "New task" }));

    expect(onOpenNewTask).toHaveBeenCalled();
  });

  // Which deployment this is, the one piece of the rail's identity block
  // that has to stay visible with the rail itself put away: an operator
  // with a staging tab and a production tab open is one tap from acting
  // on the wrong one, and the two are otherwise identical.
  it("carries the deployment's environment on the bar", () => {
    render(
      <PhoneNav
        config={{ environmentName: "staging" }}
        running={false}
        onOpenNewTask={() => {}}
      >
        <Rail />
      </PhoneNav>,
    );

    expect(screen.getByText("staging")).toBeInTheDocument();
  });
});
