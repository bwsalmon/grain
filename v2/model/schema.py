"""The model as DDL, and the derivations as views.

Two things here are not merely a translation of `types.py`, and both are
the reason `docs/data-model.md` argues a schema'd store earns its place:

**`task_state` is a view.** That document decided `TaskState` is derived
from approval plus grain's own observations and never written. As a view
there is no column to write, so no finish path can write one, no
migration can add one by accident, and "a task is in exactly one state"
stops being a rule each code path upholds and becomes a property of the
store. `types.state_of()` computes the same thing for code holding a
`Task` in hand; `v2/tests/test_model_schema.py` holds the two to agreeing.

**Declaration and observation are separate tables**, not columns on one
row. They answer to different records -- a human authors the first, grain
writes the second -- and keeping them apart is what would let a
declaration change land on a Dolt branch and be reviewed as a data diff
while observations keep being written to the trunk. Fusing them would
make every state transition a change to the reviewed thing.

**On naming.** Every table is prefixed with its entity and every
identifier is backtick-quoted, because MySQL reserves more words than is
comfortable -- `GRANT`, `READ`, `READS` are all reserved, and all three
are nouns this model wants.

**On types.** Enums are stored as their `.value` strings in `VARCHAR`
rather than as MySQL `ENUM` columns. A MySQL `ENUM` puts the vocabulary
in the schema, so adding a `LinkKind` becomes a migration; a `VARCHAR`
keeps the vocabulary in `types.py`, where the model already says it
lives. The cost is that the database will not reject an unknown value,
which `v2/tests/test_model_schema.py` covers instead.
"""

from __future__ import annotations

# Bumped whenever `TABLES` or `VIEWS` change in a way an existing database
# cannot simply be re-`CREATE ... IF NOT EXISTS`d into. `dolt.py` records
# this in `grain_schema` at init and refuses to open a database written by
# a newer one, rather than failing later with a confusing missing column.
SCHEMA_VERSION = 1

# --- declaration: what a human (or grain, proposing) asked for ---------

_TASK = """
CREATE TABLE IF NOT EXISTS `task` (
  `id`                    VARCHAR(64)  NOT NULL,
  `intent`                VARCHAR(32)  NOT NULL,
  `title`                 TEXT         NOT NULL,
  `body`                  LONGTEXT     NOT NULL DEFAULT (''),

  -- Origin: who created it, whose output they relayed, and why. The
  -- actor decides landing state; the reason never does.
  `origin_actor_kind`     VARCHAR(32)  NOT NULL,
  `origin_actor_id`       VARCHAR(255) NOT NULL,
  `origin_behalf_kind`    VARCHAR(32)  NULL,
  `origin_behalf_id`      VARCHAR(255) NULL,
  `origin_reason`         VARCHAR(32)  NOT NULL,

  -- Approval, as an Attribution. NULL is what makes a task PROPOSED, so
  -- this column is the whole difference between proposed and queued.
  `approval_actor_kind`   VARCHAR(32)  NULL,
  `approval_actor_id`     VARCHAR(255) NULL,
  `approval_behalf_kind`  VARCHAR(32)  NULL,
  `approval_behalf_id`    VARCHAR(255) NULL,

  `target_owner`          VARCHAR(255) NULL,
  `target_name`           VARCHAR(255) NULL,
  `binding`               VARCHAR(32)  NOT NULL,
  `base`                  VARCHAR(255) NULL,
  `folder`                VARCHAR(512) NULL,

  `auto_merge`            BOOLEAN      NOT NULL DEFAULT FALSE,
  -- A projection, not the identity: where this task shows up for humans.
  `external_ref`          VARCHAR(255) NULL,
  `created_at`            DATETIME(6)  NULL,
  PRIMARY KEY (`id`)
)
"""

# Read targets. A separate table rather than a JSON column because the
# question "which tasks read this repo?" is worth being able to ask, and
# because a read target grants nothing -- keeping it structurally distinct
# from `task.target` is what stops `/reads` becoming a capability
# laundering channel by accident.
_TASK_READ = """
CREATE TABLE IF NOT EXISTS `task_read` (
  `task_id`  VARCHAR(64)  NOT NULL,
  `owner`    VARCHAR(255) NOT NULL,
  `name`     VARCHAR(255) NOT NULL,
  PRIMARY KEY (`task_id`, `owner`, `name`)
)
"""

_TASK_GRANT = """
CREATE TABLE IF NOT EXISTS `task_grant` (
  `task_id`     VARCHAR(64)  NOT NULL,
  `capability`  VARCHAR(128) NOT NULL,
  -- How this task got it: label, directive, folder, or grain itself.
  `via`         VARCHAR(32)  NOT NULL,
  `folder`      VARCHAR(512) NULL,
  PRIMARY KEY (`task_id`, `capability`)
)
"""

# `blocks` is stored, not computed, even though it is a pure function of
# `kind` -- so `task_blocked` below can be a plain join rather than
# hard-coding the kind vocabulary in SQL, which would put the same fact in
# two places and let them drift.
_TASK_LINK = """
CREATE TABLE IF NOT EXISTS `task_link` (
  `task_id`  VARCHAR(64)  NOT NULL,
  `kind`     VARCHAR(32)  NOT NULL,
  `target`   VARCHAR(255) NOT NULL,
  `blocks`   BOOLEAN      NOT NULL,
  PRIMARY KEY (`task_id`, `kind`, `target`)
)
"""

_TASK_TAG = """
CREATE TABLE IF NOT EXISTS `task_tag` (
  `task_id`  VARCHAR(64)  NOT NULL,
  `tag`      VARCHAR(255) NOT NULL,
  PRIMARY KEY (`task_id`, `tag`)
)
"""

# --- observation: what grain has seen ----------------------------------

_TASK_OBSERVATION = """
CREATE TABLE IF NOT EXISTS `task_observation` (
  `task_id`                      VARCHAR(64) NOT NULL,
  `closed_at`                    DATETIME(6) NULL,
  `completed_at`                 DATETIME(6) NULL,
  -- Baselines, not beliefs: the highest comment id seen, against which a
  -- fresh read is compared. Losing one degrades rather than corrupts.
  `pending_question_comment_id`  BIGINT      NULL,
  `baseline_comment_id`          BIGINT      NULL,
  `observed_at`                  DATETIME(6) NULL,
  PRIMARY KEY (`task_id`)
)
"""

_TASK_RUN = """
CREATE TABLE IF NOT EXISTS `task_run` (
  `id`           VARCHAR(64)  NOT NULL,
  `task_id`      VARCHAR(64)  NOT NULL,
  -- The concurrency unit and the VM instance. Equal while sandboxes are
  -- long-lived; distinct once one is created per task.
  `slot`         VARCHAR(128) NOT NULL,
  `sandbox`      VARCHAR(128) NOT NULL,
  `unit`         VARCHAR(255) NULL,
  `attempt`      INT          NOT NULL DEFAULT 1,
  `started_at`   DATETIME(6)  NOT NULL,
  -- NULL means live. This is what used to be an `Assignment`.
  `finished_at`  DATETIME(6)  NULL,
  `outcome`      TEXT         NULL,
  PRIMARY KEY (`id`),
  KEY `task_run_by_task` (`task_id`),
  KEY `task_run_live` (`finished_at`)
)
"""

_LEASE = """
CREATE TABLE IF NOT EXISTS `lease` (
  `run_id`      VARCHAR(64)  NOT NULL,
  `capability`  VARCHAR(128) NOT NULL,
  `resource`    VARCHAR(512) NOT NULL,
  -- The standing credential this was minted from. Never material.
  `minted_by`   VARCHAR(128) NOT NULL,
  `issued_at`   DATETIME(6)  NOT NULL,
  `expires_at`  DATETIME(6)  NULL,
  PRIMARY KEY (`run_id`, `capability`, `resource`),
  KEY `lease_by_credential` (`minted_by`),
  KEY `lease_by_expiry` (`expires_at`)
)
"""

_GRAIN_SCHEMA = """
CREATE TABLE IF NOT EXISTS `grain_schema` (
  `id`       INT NOT NULL,
  `version`  INT NOT NULL,
  PRIMARY KEY (`id`)
)
"""

TABLES: tuple[str, ...] = (
    _TASK, _TASK_READ, _TASK_GRANT, _TASK_LINK, _TASK_TAG,
    _TASK_OBSERVATION, _TASK_RUN, _LEASE, _GRAIN_SCHEMA,
)

# --- derivations --------------------------------------------------------

# The invariant, as a view. Precedence top to bottom, matching
# `types.state_of()` exactly -- see this module's docstring on why both
# exist and `v2/tests/test_model_schema.py` on what holds them together.
_TASK_STATE = """
CREATE OR REPLACE VIEW `task_state` AS
SELECT
  `t`.`id` AS `task_id`,
  CASE
    WHEN `o`.`closed_at`    IS NOT NULL THEN 'closed'
    WHEN `o`.`completed_at` IS NOT NULL THEN 'completed'
    WHEN `o`.`pending_question_comment_id` IS NOT NULL THEN 'awaiting_reply'
    WHEN `r`.`id` IS NOT NULL THEN 'running'
    WHEN `t`.`approval_actor_kind` IS NULL THEN 'proposed'
    ELSE 'queued'
  END AS `state`
FROM `task` AS `t`
LEFT JOIN `task_observation` AS `o` ON `o`.`task_id` = `t`.`id`
LEFT JOIN `task_run` AS `r`
       ON `r`.`task_id` = `t`.`id` AND `r`.`finished_at` IS NULL
"""

# Blocked is an annotation on QUEUED, never a state of its own -- so it is
# its own view rather than another branch of the CASE above. A link blocks
# while its target task has not closed; a link whose target is not a task
# at all (a review thread, a pull request) never blocks, which falls out
# of the join rather than needing a rule.
_TASK_BLOCKED = """
CREATE OR REPLACE VIEW `task_blocked` AS
SELECT
  `l`.`task_id` AS `task_id`,
  COUNT(*)      AS `open_blockers`
FROM `task_link` AS `l`
JOIN `task` AS `bt` ON `bt`.`id` = `l`.`target`
LEFT JOIN `task_observation` AS `bo` ON `bo`.`task_id` = `l`.`target`
WHERE `l`.`blocks` = TRUE AND `bo`.`closed_at` IS NULL
GROUP BY `l`.`task_id`
"""

# What is dispatchable right now: approved, not running, nothing open in
# front of it. The same derivation beads calls `bd ready`, which this
# model arrived at independently by deciding blocked was derived.
_TASK_READY = """
CREATE OR REPLACE VIEW `task_ready` AS
SELECT `s`.`task_id` AS `task_id`
FROM `task_state` AS `s`
LEFT JOIN `task_blocked` AS `b` ON `b`.`task_id` = `s`.`task_id`
WHERE `s`.`state` = 'queued'
  AND COALESCE(`b`.`open_blockers`, 0) = 0
"""

# Every lease still outstanding, with the credential that minted it --
# which makes "what would I be breaking by rotating this?" a query.
_LEASE_LIVE = """
CREATE OR REPLACE VIEW `lease_live` AS
SELECT
  `l`.`run_id`     AS `run_id`,
  `r`.`task_id`    AS `task_id`,
  `l`.`capability` AS `capability`,
  `l`.`resource`   AS `resource`,
  `l`.`minted_by`  AS `minted_by`,
  `l`.`issued_at`  AS `issued_at`,
  `l`.`expires_at` AS `expires_at`
FROM `lease` AS `l`
JOIN `task_run` AS `r` ON `r`.`id` = `l`.`run_id`
WHERE `r`.`finished_at` IS NULL
"""

VIEWS: tuple[str, ...] = (
    _TASK_STATE, _TASK_BLOCKED, _TASK_READY, _LEASE_LIVE,
)

# The order `dolt.py` applies things in: tables, then views (a view
# referencing a missing table is an error), then the version stamp.
def statements() -> tuple[str, ...]:
    return TABLES + VIEWS
