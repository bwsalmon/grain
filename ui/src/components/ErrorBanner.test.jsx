import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import ErrorBanner from "./ErrorBanner.jsx";

describe("ErrorBanner", () => {
  it("renders the given message", () => {
    render(<ErrorBanner message="something broke" />);
    expect(screen.getByText("something broke")).toBeInTheDocument();
  });
});
