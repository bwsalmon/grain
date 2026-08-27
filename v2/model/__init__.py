"""The task model and its Dolt-backed store.

`types.py` is the model and knows nothing about storage; `schema.py` is
that model as DDL, with the derivations this project decided must not be
stored expressed as views; `sql.py` renders literals safely for a database
reached without bind parameters; `dolt.py` is the store itself.

See `docs/data-model.md` for the reasoning, and
`docs/beads-incus-feasibility.md` for why Dolt rather than files in git or
an existing tracker.
"""
