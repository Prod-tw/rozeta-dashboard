# AGENTS

## Start

Set `ADMIN_PASSWORD`, `SESSION_SECRET` (at least 32 bytes), and `EXTERNAL_API_TOKEN`, then run:

```sh
go run . -account account.csv -session session.csv -state controller-state.json
```

`-account` and `-session` are required. The default HTTP listener is `:8080`; `-state` defaults to `controller-state.json`. `compose.yaml` uses the same inputs from `/data`, persists state in the `controller-state` volume, and requires all three environment variables before creating the container.

Startup validates the account/session/OPASS/Rozeta intersection once. Malformed data, duplicate IDs, invalid retained schedule data, or remote validation failures leave the process serving only a `503` diagnostic page. A malformed or unsupported state file exits instead. Version 1 state is atomically migrated to version 2 without reconciliation or remote commands.

## Verify

- `go test ./...` runs the Go tests.
- `go test -race -shuffle=on ./...` is the CI concurrency gate.
- `pnpm test` runs the browser state-model tests.
- `pnpm format:check` checks the web assets, package metadata, and `.prettierrc`; formatting uses the repository `.prettierrc`.

## Boundaries

- `main.go` wires Gin routes, embedded `web/*` assets, authentication, startup validation, and shutdown.
- `controller.go` is the only source of desired state, lifecycle actors, observations, fencing, and reconciliation; `state.go` is only the admin WebSocket hub.
- `rozeta.go` owns authenticated Rozeta HTTP requests and complete pagination; `web/state.js` owns client epoch/revision ordering and frozen lifecycle-action payloads.
- This is one Go package at the repository root. The frontend is plain JavaScript embedded into the Go binary; there is no frontend build step.

## Controller Invariants

- State version 2 persists only desired meeting ID, generation, and automatic-Resume consumption. Lifecycle and remote observations are process-local; every restart leaves rooms `suspended / ActiveSetUnknown`.
- While active, the desired meeting must be the account's only meeting in the complete paginated `GET /api/v1/meetings?status=in_progress` result. Observe every two seconds and immediately after commands.
- Each room has one serial, coalesced `Observe -> Diff -> Act -> Requeue` path fenced by reconciliation run and desired generation. Start the desired meeting before pausing old active meetings, and only report convergence after a fresh observation proves the invariant.
- A completed desired meeting may be automatically Resumed once per generation. Persist consumption before dispatch; re-arming requires a dedicated generation and confirmation because Resume is destructive.
- Normal Stop requires an observable active set and repeatedly Pauses all active meetings until a fresh empty observation. Force-stop is available during stopping, auto-runs after 30 seconds, permits restart, and reports `RemoteOutcomeUnknown` because accepted remote commands cannot be revoked.
- Start, Stop, and Force-stop always require confirmation. Bulk requests use a frozen room list and atomically validate process epoch, room names, and expected runs before preflight or mutation.
- Goto dispatch is not route proof: `goto_meeting` followed by OBS-targeted `goto_meeting_embed` must be followed by an observation showing the desired meeting `in_progress`.
- A successful Stop's `LastStopConfirmedEmpty` is historical and stale; it does not assert that the remote active set remains empty.

## Deployment Constraint

Persistent state supports one controller replica only. Multiple replicas require leader election before deployment.
