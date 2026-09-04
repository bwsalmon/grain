import ReactMarkdown from "react-markdown";
import remarkBreaks from "remark-breaks";
import remarkGfm from "remark-gfm";

// Markdown renders a body of prose as the markdown it was almost
// certainly written in. Every agent grain dispatches writes markdown by
// default -- a relayed question comes back with headings, bullet lists,
// backticked paths and fenced diffs in it -- and rendering that as one
// flat run of text is what made a long answer hard to read
// (grain/task-93). It is used for the two places a task carries somebody
// else's words: a comment body and the task description, both of which a
// human writes in the same syntax when replying, so both get the same
// treatment rather than one rule for agents and another for people.
//
// Deliberately *not* used for an attempt's transcript
// (AttemptTranscriptOverlay): that is a log of thinking, text and tool
// calls interleaved, whose alignment is the whole of its legibility, and
// it stays in its <pre>.
//
// Nothing here renders raw HTML. react-markdown skips embedded HTML
// unless rehype-raw is added, which it is not, so a comment body -- a
// string that reaches the page straight off an agent or a GitHub user --
// cannot inject markup, and its own defaultUrlTransform already drops a
// `javascript:` href on a link or an image. That is the whole reason
// this goes through a parser rather than a hand-rolled regex pass.
//
// remarkBreaks alongside remarkGfm because these bodies are read as
// comments, not as documents: GitHub renders a single newline in a
// comment as a line break, and so does the plain-text rendering this
// replaces (.description's own white-space: pre-wrap). Without it, a
// hand-typed reply's line breaks would silently collapse into one
// paragraph -- a regression dressed up as a feature.
const REMARK_PLUGINS = [remarkGfm, remarkBreaks];

// Links go to a new tab, the same as every other outbound link in the
// UI (DetailOverlay's own pull request links): the overlay they are read
// in is a transient thing, and navigating it away loses whatever the
// operator was in the middle of.
const COMPONENTS = {
  a: ({ node, ...props }) => (
    <a {...props} target="_blank" rel="noopener noreferrer" />
  ),
};

// className is the caller's own styling hook (.description,
// .timeline-comment-body), kept on the wrapper so the markdown block
// sits exactly where the plain <div> it replaced did; .markdown carries
// the element styles for everything the parser can emit.
export default function Markdown({ children, className }) {
  return (
    <div className={className ? `markdown ${className}` : "markdown"}>
      <ReactMarkdown remarkPlugins={REMARK_PLUGINS} components={COMPONENTS}>
        {children}
      </ReactMarkdown>
    </div>
  );
}
