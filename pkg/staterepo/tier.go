package staterepo

// Which tables are worth writing out on every sync, and which are not.
//
// bwsalmon/grain#174 exported all of them every 30 seconds and committed
// whatever changed. For the tables this file calls state that is exactly
// right: they change when a human or an agent changes them, so a commit
// per change is the point of the exercise. For the handful it calls
// churn it is not. A reconcile cycle stamps task_observation.observed_at
// on every task whose pull request it is watching, every cycle, whether
// or not anything about that task moved; runs start, finish and have
// their transcripts written all day. Nobody proposes a change to those
// rows in a pull request -- they are grain's own record of what it did --
// and committing them every 30 seconds costs a rewrite of the largest
// files in the dump, forever, for a diff no reader wanted.
//
// So they are still exported, and a clone is still a complete restore.
// They are just exported on a slower clock (Config.ChurnInterval), which
// is the whole of the trade: the repository is at most one churn
// interval behind on what grain was doing, and exactly current on
// everything anybody wrote.

// Tier says how often a table's rows are worth writing out.
type Tier int

const (
	// TierState is everything a human or an agent would ever want to read
	// or change in the repository: settings, tasks, the conversation on
	// them, schedules, suites, releases. Exported on every sync.
	TierState Tier = iota
	// TierChurn is grain's own running commentary on itself. Exported on
	// Config.ChurnInterval.
	TierChurn
)

// churnTables names the tier-churn tables, and the list is deliberately
// short. The test is not "does this table ever change often" but "does a
// reconcile cycle rewrite it when nothing has actually happened":
//
//   - task_observation: observed_at and the pull request clocks are
//     stamped every cycle for every task being watched. This is the one
//     that changes on literally every tick of a deployment with anything
//     open at all.
//   - task_run: a row per attempt, plus the transcript and prompt columns
//     -- tens of kilobytes each -- written after the fact. It is the
//     largest file in the dump by an order of magnitude on any deployment
//     that has been running a while, so every commit that touches it
//     costs a rewrite of the whole thing.
//   - lease: a row per capability minted for a run, so it moves with
//     task_run and for the same reasons.
//   - task_read: which tasks a human has looked at, written as they
//     browse the UI. Not state anyone would review, and not state a
//     restore is much poorer for being an hour behind on.
//
// There is no metrics table to name here: pkg/metrics derives throughput
// and latency from task and task_run rather than materialising anything
// (pkg/model/metrics.go's own doc comment), which is why task_run is the
// table that carries the whole weight of "anything metrics-shaped".
//
// A table not named here is TierState. That default is the safe one: it
// is what every table did before this file existed, so a table a later
// build adds is exported on every sync until somebody decides otherwise.
//
// This is not SettingsTables (bind.go), and the two are not two spellings
// of one list. SettingsTables answers "which rows may be replaced
// underneath a daemon that is running", and is a short allowlist that has
// to earn every entry. This answers "how often is it worth writing a
// table out at all", and is a short denylist for the same reason in
// reverse: everything is worth writing out promptly unless it is
// something grain only ever wrote to itself. Every table in
// SettingsTables is TierState, and TierState is far wider -- task,
// task_comment and task_attachment are all things people write and none
// of them are safe to replace live.
var churnTables = map[string]bool{
	"task_observation": true,
	"task_run":         true,
	"lease":            true,
	"task_read":        true,
}

// TierOf classifies one table by name.
func TierOf(table string) Tier {
	if churnTables[table] {
		return TierChurn
	}
	return TierState
}
