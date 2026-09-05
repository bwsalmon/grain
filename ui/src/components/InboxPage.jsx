import { useEffect, useRef, useState } from "react";
import { Alert, Button, CircularProgress, TextField } from "@mui/material";
import api from "../api.js";
import { completionPhase, inboxTasks, pullRequestUrl } from "../state.js";
import { ListEmpty, ListHeader } from "./ListPrimitives.jsx";
import Markdown from "./Markdown.jsx";
import { TaskRow } from "./TaskList.jsx";

// InboxPage is the one view that is about the reader rather than about
// the work (grain/task-20): every task grain has stopped on and cannot
// go any further with until a person does something, with that person's
// own possible answers on the row itself.
//
// # Why it is not a filter of the task list
//
// It nearly is one -- state.js's waitsOnUser is a predicate over the
// same tasks the list already holds -- and that is exactly what it was
// worth not building. A filtered list shows you the tasks and then makes
// you open each one to find out what it wants: Approve lives in one
// task's own pane, Submit in another's, and a question needs the
// timeline at the bottom of a third. Five clicks a task, on the tasks
// that by definition nobody has got round to.
//
// So the row carries the answer instead. Each group below names what the
// tasks in it are waiting for and offers exactly the responses that
// apply to that wait -- Approve on a proposal, Submit on a finished
// change, Retry on a failure -- and the reply box for a parked question
// opens in place, without leaving the page. The full pane is still one
// click away on the row itself, for everything an inbox deliberately
// does not do (editing the target, reading the transcript, closing with
// its pull request).
//
// # The order the groups come in
//
// Urgency, not the life cycle STATE_ORDER describes. A parked question
// is a run stopped mid-sentence, and answering it is the only thing that
// starts it again, so it is first. A finished change waiting on Submit
// is work already paid for that is not landing. A merge the queue gave
// up on and a run that has exhausted its attempts are both stuck but
// have already had their say. Proposals come last: they are the one
// group where the honest answer is often "not now", and a long backlog
// of them should not be the first thing between a reader and the run
// that is waiting on their answer.
const GROUPS = [
  {
    id: "awaiting_reply",
    title: "Answer a question",
    hint: "A run stopped to ask something. It will not start again until it has an answer.",
    match: (t) => t.state === "awaiting_reply",
    // The one group whose response is prose rather than a verb, so it is
    // the one group that opens something (Responder below) instead of
    // firing a request.
    reply: true,
  },
  {
    id: "awaiting_submit",
    title: "Submit for merge",
    hint: "The run is over and its pull request is on no queue. Nothing lands it until you submit it.",
    match: (t) => t.state === "awaiting_submit",
    actions: [
      {
        label: "Submit",
        variant: "contained",
        post: "/submit",
        title:
          "Put this pull request on grain's merge queue: it is merged once its checks pass.",
      },
    ],
  },
  {
    id: "merge_blocked",
    title: "Unblock a merge",
    hint: "The merge queue gave up on landing these automatically -- its own comment on each says why.",
    // completionPhase is the whole of this test: it is non-null for a
    // completed task the queue has stopped driving and null for the
    // ordinary one sitting on it (state.js). Such a task's badge still
    // reads "Queued for merge", which is why of everything in this
    // inbox it is the case nobody finds by scrolling the list.
    match: (t) => t.state === "completed" && completionPhase(t) !== null,
    actions: [
      {
        label: "Open pull request",
        href: (t) => (t.pullRequest ? pullRequestUrl(t.pullRequest) : null),
        title: "Sort it out on GitHub by hand.",
      },
    ],
  },
  {
    id: "failed",
    title: "Retry or close",
    hint: "Dispatch has given up on these after too many failures in a row. Retry is the only way back into the queue.",
    match: (t) => t.state === "failed",
    actions: [
      {
        label: "Retry",
        variant: "contained",
        post: "/retry",
        title: "Clear the failure streak and put this task back in the queue.",
      },
      {
        label: "Close",
        color: "error",
        post: "/close",
        confirm: (t) => `Close ${t.id}? It can be reopened later.`,
        title: "Give up on this task. Reopen puts it back.",
      },
    ],
  },
  {
    id: "proposed",
    title: "Approve a proposal",
    hint: "An agent proposed these. Nothing dispatches them until you approve one.",
    match: (t) => t.state === "proposed",
    actions: [
      {
        label: "Approve",
        variant: "contained",
        post: "/approve",
        title: "Let this task join the queue and be dispatched.",
      },
      {
        label: "Decline",
        color: "error",
        post: "/close",
        confirm: (t) => `Close ${t.id} without running it? It can be reopened.`,
        title: "Close it unrun. Reopen puts it back among the proposals.",
      },
    ],
  },
];

// groupTasks assigns every waiting task to the first group that claims
// it, so no task is ever listed twice -- a task can be both awaiting a
// submit and carrying a merge-blocked mark, and it belongs under the one
// thing a reader can do about it first.
//
// Anything waitsOnUser admits that no group claims would vanish here,
// which is the one way these two can drift apart; InboxPage.test.jsx
// checks that they don't.
function groupTasks(tasks) {
  return GROUPS.map((g) => ({
    group: g,
    tasks: tasks.filter(
      (t) => GROUPS.find((candidate) => candidate.match(t)) === g,
    ),
  })).filter((row) => row.tasks.length > 0);
}

export default function InboxPage({
  tasks,
  config,
  onOpenTask,
  onAct,
  showError,
}) {
  // Which task's reply box is open, or null. One at a time: the box is
  // as tall as the question it is answering, and two of them open at
  // once turns a list of what is waiting into a page of forms.
  const [replyTo, setReplyTo] = useState(null);
  const waiting = inboxTasks(tasks);
  const groups = groupTasks(waiting);

  // act wraps App's own runner so a row's button can say what it did
  // before firing: the confirm is here rather than in onAct because only
  // the button knows whether its own action is the kind worth asking
  // about.
  const run = (t, action) => {
    if (action.confirm && !confirm(action.confirm(t))) return;
    onAct(() => api(`/api/tasks/${t.id}${action.post}`, { method: "POST" }));
  };

  return (
    <main className="inbox">
      <ListHeader title="Inbox" count={waiting.length} />
      {groups.length === 0 && (
        <ListEmpty>
          Nothing is waiting on you. Every task is either running, queued or
          done.
        </ListEmpty>
      )}
      {groups.map(({ group, tasks: rows }) => (
        <section className="inbox-group" key={group.id}>
          <div className="inbox-group-header">
            <h3>{group.title}</h3>
            <span className="count">{rows.length}</span>
            <p className="inbox-group-hint">{group.hint}</p>
          </div>
          <ul className="task-list">
            {rows.map((t) => (
              <li className="inbox-item" key={t.id}>
                <div className="inbox-line">
                  <TaskRow t={t} config={config} onOpenTask={onOpenTask} />
                  <div className="inbox-actions">
                    {group.reply && (
                      <Button
                        size="small"
                        variant={replyTo === t.id ? "outlined" : "contained"}
                        onClick={() =>
                          setReplyTo(replyTo === t.id ? null : t.id)
                        }
                      >
                        {replyTo === t.id ? "Cancel" : "Reply"}
                      </Button>
                    )}
                    {(group.actions || []).map((action) =>
                      action.href ? (
                        <Button
                          key={action.label}
                          size="small"
                          variant={action.variant || "outlined"}
                          color={action.color}
                          title={action.title}
                          href={action.href(t) || undefined}
                          disabled={!action.href(t)}
                          target="_blank"
                          rel="noopener noreferrer"
                        >
                          {action.label}
                        </Button>
                      ) : (
                        <Button
                          key={action.label}
                          size="small"
                          variant={action.variant || "outlined"}
                          color={action.color}
                          title={action.title}
                          onClick={() => run(t, action)}
                        >
                          {action.label}
                        </Button>
                      ),
                    )}
                  </div>
                </div>
                {group.reply && replyTo === t.id && (
                  <Responder
                    t={t}
                    onAct={onAct}
                    onOpenTask={onOpenTask}
                    onSent={() => setReplyTo(null)}
                    showError={showError}
                  />
                )}
              </li>
            ))}
          </ul>
        </section>
      ))}
    </main>
  );
}

// Responder is the reply box for one parked task, plus the question it
// is answering -- the only part of a task's own pane this page brings
// with it, because a reply written without the question in front of you
// is a guess.
//
// It fetches the task's detail on open rather than reading the row it
// hangs off: the list shape carries neither the conversation nor
// pendingSecret (pkg/ui's Task vs TaskDetail), and both matter here. The
// fetch is per opened row and only on open, so a page of forty waiting
// tasks still costs the one list poll everything else on screen costs.
//
// pendingSecret is why this is a fetch and not a textarea. A run that
// asked for a credential (mcp's request_secret) parks in exactly the
// same state as one that asked a question, and a reply is a comment --
// stored in the conversation and fed to the next run's prompt. Offering
// an undifferentiated box here would invite somebody to paste a
// credential into it. So a task parked on a secret gets no box at all on
// this page: it gets the sentence saying what it wants and a way through
// to the write-only field on its own pane (DetailOverlay's
// PendingSecret), which is the one place a value can be typed without
// ending up in the task.
function Responder({ t, onAct, onOpenTask, onSent, showError }) {
  const [detail, setDetail] = useState(null);
  const [failed, setFailed] = useState(false);
  // Uncontrolled, for the reason Timeline's own reply box is
  // (DetailOverlay.jsx): App re-renders this page every poll, and a
  // draft nobody has sent yet should survive that.
  const bodyRef = useRef(null);

  useEffect(() => {
    let live = true;
    api(`/api/tasks/${t.id}`)
      .then((d) => {
        if (live) setDetail(d);
      })
      .catch((err) => {
        if (!live) return;
        setFailed(true);
        showError(err);
      });
    return () => {
      live = false;
    };
  }, [t.id, showError]);

  const send = () => {
    const body = bodyRef.current?.value || "";
    if (!body.trim()) return;
    onAct(() =>
      api(`/api/tasks/${t.id}/comments`, {
        method: "POST",
        body: JSON.stringify({ body }),
      }),
    );
    onSent();
  };

  if (failed) {
    return (
      <div className="inbox-responder">
        <Button size="small" onClick={() => onOpenTask(t.id)}>
          Open the task to reply
        </Button>
      </div>
    );
  }
  if (detail === null) {
    return (
      <div className="inbox-responder">
        <CircularProgress size={16} />
      </div>
    );
  }
  if (detail.pendingSecret) {
    return (
      <div className="inbox-responder">
        <Alert severity="info" icon={false}>
          This run asked for a credential ({detail.pendingSecret.name}), not for
          an answer. It is set on the task&apos;s own pane, in a write-only box
          that puts the value straight into grain&apos;s secret store instead of
          into this conversation.
          <div className="inbox-responder-row">
            <Button
              size="small"
              variant="contained"
              onClick={() => onOpenTask(t.id)}
            >
              Open the task
            </Button>
          </div>
        </Alert>
      </div>
    );
  }
  // The question itself is the last thing said on the task. grain relays
  // an agent's ask_question as a comment and parks the task on it
  // (orchestrator's own relayComment), so on a task in this state the
  // last comment is that question -- and on the rare one where it is
  // not, it is still the thing a reply is answering.
  const question = (detail.comments || [])[detail.comments.length - 1];
  return (
    <div className="inbox-responder">
      {question ? (
        <Markdown className="inbox-question">{question.body}</Markdown>
      ) : (
        <p className="hint">
          This run parked without leaving a question behind it.
        </p>
      )}
      <div className="inbox-responder-row">
        <TextField
          multiline
          rows={2}
          size="small"
          fullWidth
          autoFocus
          placeholder="Reply..."
          inputRef={bodyRef}
          inputProps={{ "aria-label": `Reply to ${t.id}` }}
        />
        <Button variant="contained" onClick={send}>
          Send
        </Button>
      </div>
    </div>
  );
}
