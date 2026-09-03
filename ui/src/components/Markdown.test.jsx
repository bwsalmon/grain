import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import Markdown from "./Markdown.jsx";

describe("Markdown", () => {
  it("renders the block markdown an agent writes as real elements", () => {
    const { container } = render(
      <Markdown>{"## What I found\n\n- the `retry` path never resets\n- and [the PR](https://example.com/pr/1) shows it\n"}</Markdown>
    );

    expect(screen.getByRole("heading", { name: "What I found" })).toBeInTheDocument();
    expect(container.querySelectorAll("li")).toHaveLength(2);
    expect(screen.getByText("retry").tagName).toBe("CODE");
  });

  it("renders emphasis, fenced code and tables", () => {
    const { container } = render(
      <Markdown>{"**bold** and *italic*\n\n```go\nfunc main() {}\n```\n\n| a | b |\n| - | - |\n| 1 | 2 |\n"}</Markdown>
    );

    expect(container.querySelector("strong")).toHaveTextContent("bold");
    expect(container.querySelector("em")).toHaveTextContent("italic");
    expect(container.querySelector("pre code")).toHaveTextContent("func main() {}");
    // The table needs remark-gfm: without it those pipes are one
    // paragraph of literal text.
    expect(container.querySelectorAll("table th")).toHaveLength(2);
  });

  // Reading a comment shouldn't navigate the overlay it is being read in
  // away, the same as every other outbound link in the UI.
  it("opens links in a new tab, safely", () => {
    render(<Markdown>{"see [the run](https://example.com/run)"}</Markdown>);

    const link = screen.getByRole("link", { name: "the run" });
    expect(link).toHaveAttribute("href", "https://example.com/run");
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", "noopener noreferrer");
  });

  // A body reaches this straight off an agent or a GitHub user, so the
  // two ways markdown can carry markup have to stay inert: react-markdown
  // renders no raw HTML without rehype-raw, and drops a javascript: href.
  it("does not render raw HTML or a javascript: link", () => {
    const { container } = render(
      <Markdown>{"<img src=x onerror=\"alert(1)\">\n\n[click](javascript:alert(1))"}</Markdown>
    );

    expect(container.querySelector("img")).toBeNull();
    // The <a> survives, stripped of its href: what must not survive is
    // anything a click could execute.
    expect(container.querySelector("a")).toHaveTextContent("click");
    expect(container.querySelector("a")).toHaveAttribute("href", "");
  });

  // remark-breaks: a hand-typed reply's own line breaks are meaningful,
  // and markdown would otherwise fold these two lines into one paragraph
  // of running text.
  it("keeps a single newline as a line break", () => {
    const { container } = render(<Markdown>{"first line\nsecond line"}</Markdown>);

    expect(container.querySelectorAll("p")).toHaveLength(1);
    expect(container.querySelector("br")).toBeInTheDocument();
  });

  it("puts the caller's own class beside its own on the wrapper", () => {
    const { container } = render(<Markdown className="timeline-comment-body">{"hi"}</Markdown>);

    expect(container.firstChild).toHaveClass("markdown", "timeline-comment-body");
  });
});
