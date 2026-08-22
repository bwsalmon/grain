"""A text UI for browsing past dispatch sessions by trigger and reading
their captured trajectory (docs/roadmap.md item 10) — reachable over SSH
alone, since `docs/design.md` is explicit that "the only inbound port on
this host is SSH" and there is no web UI to build against instead. Wired in
as `grain sessions browse` (`grain/cli.py`); `grain sessions list` covers
the same data non-interactively, for a script or a plain SSH session that
doesn't want a full-screen app.

**stdlib `curses`, not a richer TUI library.** `pyproject.toml` is
deliberately dependency-free ("this runs on a stock Debian host"), and
docs/roadmap.md item 10 names this exact tradeoff as the first real
candidate for breaking that convention. `curses` ships with CPython on
every POSIX target this project runs on (the controller is always Debian —
docs/design.md; this is an admin-only tool, never something a sandbox
runs) and needs nothing installed. A richer library (`textual`, `urwid`)
would be the *first* third-party dependency this repo takes on, for a
browse-only tool used occasionally by an operator over SSH — a real trade,
weighed here rather than defaulted past: `curses`'s rougher edges (manual
layout, no mouse/scrollback niceties, more code for the same feature) are
worth paying to keep the dependency-free property intact, since nothing
about this tool's job — list, filter, select, scroll text — pushes past
what `curses` does perfectly well. If a future need genuinely outgrows
that (richer widgets, a non-POSIX controller), that is the point to
reconsider — not asserted here as settled forever.

**Split deliberately, because curses screens are hard to unit test
directly**: `SessionListState` (which rows are visible, in what order,
under which filter, which is selected) and the `format_*` helpers are
plain data manipulation with no `curses` import at all, covered by
`tests/test_automation_tui.py` with no terminal involved. `_draw_*`/`run`
below are the actual curses mechanics — screen coordinates, `getch()`,
the event loop — exercised only by hand (`grain sessions browse`), per this
task's own guidance that curses rendering isn't worth contorting into a
unit test.
"""

from __future__ import annotations

import curses
from dataclasses import dataclass
from enum import Enum

from .history import FileSessionHistory, SessionRecord
from .state import TriggerKind
from .transcript import TranscriptEvent, parse_transcript


class KindFilter(Enum):
    ALL = "all"
    ISSUE = "issue"
    PR = "pr"

    def next(self) -> "KindFilter":
        order = list(KindFilter)
        return order[(order.index(self) + 1) % len(order)]


class OutcomeFilter(Enum):
    ALL = "all"
    SUCCEEDED = "succeeded"
    FAILED = "failed"
    STRANDED = "stranded"

    def next(self) -> "OutcomeFilter":
        order = list(OutcomeFilter)
        return order[(order.index(self) + 1) % len(order)]


@dataclass
class SessionListState:
    """Everything about "what rows does the list view show right now" —
    no curses. `records` is expected already sorted newest-first
    (`FileSessionHistory.all()`'s own contract).
    """

    records: list[SessionRecord]
    kind_filter: KindFilter = KindFilter.ALL
    outcome_filter: OutcomeFilter = OutcomeFilter.ALL
    selected: int = 0

    def visible(self) -> list[SessionRecord]:
        rows = self.records
        if self.kind_filter is KindFilter.ISSUE:
            rows = [r for r in rows if r.kind is TriggerKind.ISSUE]
        elif self.kind_filter is KindFilter.PR:
            rows = [r for r in rows if r.kind is TriggerKind.PR]
        if self.outcome_filter is not OutcomeFilter.ALL:
            rows = [r for r in rows if r.outcome == self.outcome_filter.value]
        return rows

    def cycle_kind(self) -> None:
        self.kind_filter = self.kind_filter.next()
        self.selected = 0

    def cycle_outcome(self) -> None:
        self.outcome_filter = self.outcome_filter.next()
        self.selected = 0

    def move(self, delta: int) -> None:
        rows = self.visible()
        if not rows:
            self.selected = 0
            return
        self.selected = max(0, min(len(rows) - 1, self.selected + delta))

    def selected_record(self) -> SessionRecord | None:
        rows = self.visible()
        if not rows or self.selected >= len(rows):
            return None
        return rows[self.selected]


def format_row(record: SessionRecord, *, width: int = 0) -> str:
    trigger = f"{record.kind.value}#{record.issue}"
    when = record.started_at.strftime("%Y-%m-%d %H:%M")
    captured = "yes" if record.transcript_path else "no"
    row = (
        f"{when}  {trigger:<10} {record.outcome:<10} {record.sandbox:<12} "
        f"transcript={captured:<3} {record.unit}"
    )
    return row[:width] if width else row


def format_header(state: SessionListState) -> str:
    return (
        f"kind={state.kind_filter.value}  outcome={state.outcome_filter.value}  "
        f"({len(state.visible())}/{len(state.records)} shown)   "
        "[k] kind filter  [o] outcome filter  [enter] view  [q] quit"
    )


@dataclass
class DetailState:
    """The trajectory-view screen's own "what's visible right now" — a
    parsed event list plus a scroll offset. Same no-curses split as
    `SessionListState`.
    """

    record: SessionRecord
    events: list[TranscriptEvent]
    scroll: int = 0

    @classmethod
    def load(cls, history: FileSessionHistory, record: SessionRecord) -> "DetailState":
        text = history.read_transcript(record)
        return cls(record=record, events=parse_transcript(text))

    def scroll_by(self, delta: int) -> None:
        if not self.events:
            self.scroll = 0
            return
        self.scroll = max(0, min(len(self.events) - 1, self.scroll + delta))


def format_detail_title(record: SessionRecord) -> str:
    return (
        f"{record.kind.value}#{record.issue}  sandbox={record.sandbox}  "
        f"outcome={record.outcome}  started={record.started_at.isoformat()}"
    )


# --- curses mechanics -- not unit tested, see this module's own docstring --

def _draw_list(stdscr, state: SessionListState) -> None:
    stdscr.erase()
    height, width = stdscr.getmaxyx()
    stdscr.addnstr(0, 0, format_header(state), max(width, 1), curses.A_BOLD)
    rows = state.visible()
    for i, record in enumerate(rows):
        y = i + 2
        if y >= height:
            break
        attr = curses.A_REVERSE if i == state.selected else curses.A_NORMAL
        stdscr.addnstr(y, 0, format_row(record, width=width), width, attr)
    if not rows:
        stdscr.addnstr(2, 0, "(no sessions recorded yet)", width)
    stdscr.refresh()


def _draw_detail(stdscr, state: DetailState) -> None:
    stdscr.erase()
    height, width = stdscr.getmaxyx()
    stdscr.addnstr(0, 0, format_detail_title(state.record), width, curses.A_BOLD)
    stdscr.addnstr(1, 0, "[up/down] scroll  [q] back", width)
    if not state.events:
        stdscr.addnstr(3, 0, "(no trajectory was captured for this session)", width)
    else:
        for i, event in enumerate(state.events[state.scroll:]):
            y = i + 3
            if y >= height:
                break
            stdscr.addnstr(y, 0, event.summary, width)
    stdscr.refresh()


def _detail_loop(stdscr, history: FileSessionHistory, record: SessionRecord) -> None:
    state = DetailState.load(history, record)
    while True:
        _draw_detail(stdscr, state)
        key = stdscr.getch()
        if key in (ord("q"), 27):
            return
        elif key == curses.KEY_UP:
            state.scroll_by(-1)
        elif key == curses.KEY_DOWN:
            state.scroll_by(1)


def _list_loop(stdscr, history: FileSessionHistory) -> None:
    curses.curs_set(0)
    state = SessionListState(records=history.all())
    while True:
        _draw_list(stdscr, state)
        key = stdscr.getch()
        if key in (ord("q"), 27):
            return
        elif key == ord("k"):
            state.cycle_kind()
        elif key == ord("o"):
            state.cycle_outcome()
        elif key == curses.KEY_UP:
            state.move(-1)
        elif key == curses.KEY_DOWN:
            state.move(1)
        elif key in (curses.KEY_ENTER, 10, 13):
            record = state.selected_record()
            if record is not None:
                _detail_loop(stdscr, history, record)
                # Coming back from the detail view: re-read history, since a
                # sweep could have recorded a new session while the operator
                # was reading an old one.
                state.records = history.all()


def run(history: FileSessionHistory) -> None:
    curses.wrapper(_list_loop, history)
