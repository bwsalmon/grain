package model

// What happened *inside* a run, kept as data.
//
// Everything else pkg/metrics measures is derived from moments already
// recorded for other reasons -- filed, approved, dispatched, finished --
// and this file is the one exception the rules in that package's own doc
// comment allow: it stores rows, because what it stores is not derivable
// from anything that survives the run.
//
// The census itself is not new. orchestrator.toolCallSummary has always
// counted every tool a run called and how many of those calls came back
// as errors, for every run including the successful ones -- and then
// rendered it into English and put it in task_run.detail, a column a
// human reads one row at a time. Nothing could aggregate it, because it
// was prose; agent.Result, where the numbers came from, is discarded the
// moment the run returns; and Result.Transcript is per-framework prose
// that a tool's own output can forge (outcomeOf's doc comment). So "is
// edit_file's error rate climbing?" and "how big does a run_command
// answer actually get?" -- the questions that would size a truncation cap
// or find a sandbox going bad -- had no answer anywhere.
//
// Two tables, because the run holds two different kinds of fact:
//
//   - task_run_tool is the census, one row per run per tool. Counts and
//     sizes only; nothing here is an argument, a result or a snippet of
//     output, so no part of a task's contents lands in a measurement
//     table.
//   - task_run_check_wait is one row per wait_for_checks call, because
//     the CI loop the prompt sends every run around is a sequence rather
//     than a total: which verdict each wait ended in, how long it
//     blocked, and how many pushes came before it are three facts about
//     one call, and averaging them into a per-run row would lose the one
//     question worth asking (how many pushes it took to go green).
//
// Both are written once, by orchestrator.RunDispatch, after the run has
// already finished -- the same after-the-fact shape SetRunTranscript has
// -- and never updated afterwards.

import (
	"context"
	"database/sql"
	"fmt"
	"math/bits"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SizeHistogram counts tool results by size, in base-2 buckets: bucket e
// holds every result of [2^(e-1), 2^e) bytes, and bucket 0 holds the
// empty ones.
//
// It exists because a percentile needs a distribution and a distribution
// needs every sample, and a row per tool *call* is the one storage shape
// this was not worth: a busy run makes hundreds, and pkg/metrics reads
// its tables whole (Store.TaskTimings' own doc comment on why). Twenty-odd
// small integers per run per tool answer the question that matters --
// "what cap would have kept 95% of these results whole?" -- to within a
// factor of two, and metrics.Sizes names its percentiles AtMost so that
// the factor of two is stated rather than implied.
//
// A map rather than a slice because it is sparse: a tool whose results
// are all a few hundred bytes touches one or two buckets.
type SizeHistogram map[int]int

// SizeBucket is the bucket a result of n bytes falls in.
func SizeBucket(n int64) int {
	if n <= 0 {
		return 0
	}
	// bits.Len is the smallest e with n < 2^e, which is exactly the
	// bucket's own definition.
	return bits.Len64(uint64(n))
}

// SizeBucketMax is the largest result bucket e can hold -- the bound a
// percentile drawn from this histogram is honest to.
func SizeBucketMax(e int) int64 {
	if e <= 0 {
		return 0
	}
	return int64(1)<<uint(e) - 1
}

// Add records one result of n bytes.
func (h SizeHistogram) Add(n int64) { h[SizeBucket(n)]++ }

// Merge adds every count in other into h, which is how a report over many
// runs gets one distribution out of a row per run.
func (h SizeHistogram) Merge(other SizeHistogram) {
	for bucket, count := range other {
		h[bucket] += count
	}
}

// Total is how many results the histogram counts.
func (h SizeHistogram) Total() int {
	n := 0
	for _, count := range h {
		n += count
	}
	return n
}

// Buckets is the occupied buckets, smallest first -- the order anything
// walking a distribution needs them in.
func (h SizeHistogram) Buckets() []int {
	out := make([]int, 0, len(h))
	for bucket := range h {
		out = append(out, bucket)
	}
	sort.Ints(out)
	return out
}

// Encode renders the histogram for its column: "bucket:count" pairs,
// comma separated, smallest bucket first. Empty for a histogram with
// nothing in it, so an empty column and an empty histogram are the same
// thing in both directions.
//
// A text column rather than a table of its own: these counts are only
// ever read as a whole, alongside the row they belong to, and never
// queried or joined on.
func (h SizeHistogram) Encode() string {
	if len(h) == 0 {
		return ""
	}
	parts := make([]string, 0, len(h))
	for _, bucket := range h.Buckets() {
		parts = append(parts, strconv.Itoa(bucket)+":"+strconv.Itoa(h[bucket]))
	}
	return strings.Join(parts, ",")
}

// DecodeSizeHistogram reads Encode's output back. A malformed pair is
// dropped rather than failing the read: this is a measurement, and a
// report that refuses to render because one historical row is unreadable
// is worse than one that measures the rest.
func DecodeSizeHistogram(text string) SizeHistogram {
	h := SizeHistogram{}
	for _, pair := range strings.Split(text, ",") {
		bucket, count, ok := strings.Cut(pair, ":")
		if !ok {
			continue
		}
		b, err := strconv.Atoi(strings.TrimSpace(bucket))
		if err != nil || b < 0 {
			continue
		}
		c, err := strconv.Atoi(strings.TrimSpace(count))
		if err != nil || c <= 0 {
			continue
		}
		h[b] += c
	}
	return h
}

// RunToolUse is one run's whole use of one tool: how often it called it,
// how often that came back an error, how often the tool's own bound ended
// the call rather than the call finishing, and how big the answers were.
//
// TimedOut is only ever non-zero for a tool that bounds its own work and
// says so -- run_command, today (mcp.RunCommandTimedOut). It is a count
// of calls grain itself cut off, which is a different thing from a call
// that failed: the first sizes a timeout, the second is the work.
type RunToolUse struct {
	RunID          string
	Tool           string
	Calls          int
	Errored        int
	TimedOut       int
	ResultBytes    int64
	MaxResultBytes int64
	Sizes          SizeHistogram
}

// RunCheckWait is one wait_for_checks call: how it ended, how long it
// blocked, and how many pushes this run had made before it.
//
// PushesBefore is what makes "how many pushes does a run take to go
// green?" answerable -- read off the first wait in a run that ended
// WaitVerdictPassed. It is stored rather than derived for the same reason
// the census is: the run's own sequence of tool calls does not survive it.
type RunCheckWait struct {
	RunID        string
	Seq          int
	Verdict      string
	Waited       time.Duration
	PushesBefore int
}

// RunTelemetry is everything one run recorded about its own tool use --
// what RecordRunTelemetry writes in a single transaction, so a report
// never sees a run's census half-written.
type RunTelemetry struct {
	Tools      []RunToolUse
	CheckWaits []RunCheckWait
}

// Empty reports whether there is nothing to record -- a run that never
// reached its agent, or one that made no tool calls at all. Such a run
// gets no rows rather than a row of zeroes: "this run called nothing" is
// already recorded, in task_run.detail and in its outcome, and a zero row
// here would only dilute every per-tool rate it was averaged into.
func (t RunTelemetry) Empty() bool { return len(t.Tools) == 0 && len(t.CheckWaits) == 0 }

// RecordRunTelemetry writes runID's census, replacing anything already
// recorded for that run.
//
// Replacing rather than adding: the write happens once per run today, and
// a retry of it (or a recovery path that runs it again) must leave one
// census rather than two, since every count here is summed.
//
// Recording it must never cost a run -- the caller logs a failure and
// carries on, the same rule SetRunAgentStarted's own doc comment states:
// a measurement that cannot be taken is not a reason to fail the work
// being measured.
func (s *Store) RecordRunTelemetry(ctx context.Context, runID string, t RunTelemetry) error {
	return s.write(ctx, "record run "+runID+" telemetry", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM `task_run_tool` WHERE `run_id` = ?", runID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM `task_run_check_wait` WHERE `run_id` = ?", runID); err != nil {
			return err
		}
		for _, use := range t.Tools {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO `task_run_tool` (`run_id`,`tool`,`calls`,`errored`,`timed_out`,"+
					"`result_bytes`,`max_result_bytes`,`size_buckets`) VALUES (?,?,?,?,?,?,?,?)",
				runID, use.Tool, use.Calls, use.Errored, use.TimedOut,
				use.ResultBytes, use.MaxResultBytes, use.Sizes.Encode()); err != nil {
				return fmt.Errorf("recording %s's use of %s: %w", runID, use.Tool, err)
			}
		}
		for _, wait := range t.CheckWaits {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO `task_run_check_wait` (`run_id`,`seq`,`verdict`,`waited_ms`,`pushes_before`) "+
					"VALUES (?,?,?,?,?)",
				runID, wait.Seq, wait.Verdict,
				wait.Waited.Milliseconds(), wait.PushesBefore); err != nil {
				return fmt.Errorf("recording %s's CI wait %d: %w", runID, wait.Seq, err)
			}
		}
		return nil
	})
}

// RunToolUses is every census row ever recorded, oldest run first -- the
// whole table, for the reason TaskTimings gives: a report's window bounds
// which measurements it covers, not which rows it may read, and these
// rows carry no time of their own to filter on anyway. They are windowed
// against their run's own finished_at, which pkg/metrics joins in memory
// by RunTiming.RunID.
func (s *Store) RunToolUses(ctx context.Context) ([]RunToolUse, error) {
	var out []RunToolUse
	err := each(ctx, s.db,
		"SELECT `run_id`,`tool`,`calls`,`errored`,`timed_out`,`result_bytes`,"+
			"`max_result_bytes`,`size_buckets` FROM `task_run_tool` "+
			"ORDER BY `run_id`, `tool`", nil,
		func(rows *sql.Rows) error {
			var use RunToolUse
			var buckets sql.NullString
			if err := rows.Scan(&use.RunID, &use.Tool, &use.Calls, &use.Errored, &use.TimedOut,
				&use.ResultBytes, &use.MaxResultBytes, &buckets); err != nil {
				return err
			}
			use.Sizes = DecodeSizeHistogram(buckets.String)
			out = append(out, use)
			return nil
		})
	return out, err
}

// RunCheckWaits is every wait_for_checks call ever recorded, in the order
// each run made them -- see RunToolUses on why this is the whole table.
func (s *Store) RunCheckWaits(ctx context.Context) ([]RunCheckWait, error) {
	var out []RunCheckWait
	err := each(ctx, s.db,
		"SELECT `run_id`,`seq`,`verdict`,`waited_ms`,`pushes_before` "+
			"FROM `task_run_check_wait` ORDER BY `run_id`, `seq`", nil,
		func(rows *sql.Rows) error {
			var wait RunCheckWait
			var waitedMS int64
			if err := rows.Scan(&wait.RunID, &wait.Seq, &wait.Verdict, &waitedMS,
				&wait.PushesBefore); err != nil {
				return err
			}
			wait.Waited = time.Duration(waitedMS) * time.Millisecond
			out = append(out, wait)
			return nil
		})
	return out, err
}
