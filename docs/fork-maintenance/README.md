# Fork Maintenance Documentation

This directory contains the design rationale, research, and audit history
for the NovusOrdo Xray-core fork. See also [../../FORK-MAINTENANCE.md](../../FORK-MAINTENANCE.md)
for the top-level maintenance guide.

## Directory layout

```
docs/fork-maintenance/
├── README.md                   (this file)
├── research/                   — "why" docs for each custom feature
├── verification/               — audit reports of the rebase onto upstream v26.4.15
└── rebase-2026-04-17/          — historical snapshot of the pre-rebase delta
```

## research/

Design documents explaining the intent and implementation of each custom
feature. Written during the rebase from v25.12.8 → v26.4.15 as reference
material for re-implementing the features on top of the new upstream base.

| Doc | Feature |
|-----|---------|
| [01-remote-validator.md](research/01-remote-validator.md) | Remote tower validator + wildcard clients + event notifications |
| [02-xerr-error-response.md](research/02-xerr-error-response.md) | XERR structured error response protocol |
| [03-auth-verified-client-version.md](research/03-auth-verified-client-version.md) | AuthVerified + ClientVersion VLESS addons |
| [04-online-users-metrics.md](research/04-online-users-metrics.md) | `/online` and `/online-users` HTTP endpoints |
| [05-relay-mode-vlessroute.md](research/05-relay-mode-vlessroute.md) | Relay mode + VlessRoute bytes 8:10 |

These docs are the source of truth for what each feature does and why. Update
them when behavior changes.

## verification/

Per-feature audit reports from the April 2026 rebase — each one walks through
the "should exist" list from the corresponding research doc and confirms
presence in the rebased codebase.

Verdicts after all follow-up fixes:

| Feature | Verdict |
|---------|---------|
| 01 Remote validator | PASSED (after fix to dispatcher outer guard) |
| 02 XERR errors | PASSED |
| 03 AuthVerified + ClientVersion | PASSED (after fix to addons.go encoder) |
| 04 Online metrics | PASSED |
| 05 Relay mode | PASSED |

## rebase-2026-04-17/

Historical snapshot of the fork's state right before the rebase:

- `00-all-custom-changes.diff` — unified diff of all 27 custom commits (the
  "what was in the fork" reference, frozen at rebase time)
- `00-commit-history.txt` — one-line log of the 27 custom commits
- `00-commit-history-detailed.txt` — per-commit file stats

Useful for historical reference and if a future sync needs to re-check what
the fork's intent was at a given time.

## Updating these docs

- **Adding a new custom feature** → add a research doc explaining what/why.
- **Modifying an existing custom feature** → update the relevant research
  doc to reflect new behavior.
- **Syncing from upstream** → create a new `rebase-YYYY-MM-DD/` directory
  with the delta snapshot, and re-run the verification audit for every
  feature, committing new reports to `verification/`.
