# AGENTS

## Overview

- `go run . -account account.csv -session session.csv -state controller-state.json` starts Gin, the admin WebSocket, and the desired-state controller. Every room starts `suspended / ActiveSetUnknown` until an administrator starts it.
- `controller.go` owns persisted desired meeting generations, lifecycle actors, active-set observation, command fencing, and reconciliation. `rozeta.go` owns Rozeta HTTP calls. `state.go` is only the admin WebSocket hub.
- The version 2 state file is authoritative for meeting ID, generation, and automatic-Resume consumption. Lifecycle and observations are process-local and never persisted.
- The active invariant is that the desired meeting is the account's only meeting returned by the complete paginated `status=in_progress` query.

## Start

```sh
export ADMIN_PASSWORD='replace-with-a-strong-password'
export SESSION_SECRET='replace-with-at-least-32-random-bytes'
export EXTERNAL_API_TOKEN='replace-with-a-long-machine-token'
go run . -account account.csv -session session.csv -state controller-state.json
```

The account CSV is required. The session CSV is optional and only recommends and orders meetings; it never selects desired state. A room without persisted desired state remains `InitialMeetingRequired`. An existing malformed or unsupported state file stops startup. Version 1 state is atomically migrated to version 2 by preserving meeting ID and generation and dropping `running`; migration performs no reconciliation or remote command.

## Debug

- `go test ./...` runs tests.
- `go test -race -shuffle=on ./...` checks controller concurrency.
- `pnpm test` checks frontend epoch, revision, and authoritative snapshot ordering.
- Inspect lifecycle, reconciliation run, desired generation and status, complete active meeting IDs, observation staleness, conditions, recent actions, and errors.
- Goto dispatch does not prove a route change. A fresh active-set observation proves route correctness only when desired appears `in_progress`.
- Suspended observations are historical and stale. `LastStopConfirmedEmpty` records a successful Stop but never claims the remote set remains empty.

## Runtime Facts

- Lifecycle states are `suspended`, `starting`, `active`, and `stopping`; lifecycle is never restored from disk.
- Admins may update suspended desired state without remote commands. Active desired updates reconcile immediately and require destructive preflight when selecting a completed meeting.
- Every Start, Stop, and Force-stop action requires confirmation. Browser-captured bulk actions use a frozen room list and atomically validate epoch, names, and expected runs before preflight or state changes.
- Start preflight observes desired status and the complete active set. Start All may activate observable rooms while leaving rooms with failed preflight suspended.
- Each active room runs one serial, coalesced `Observe -> Diff -> Act -> Requeue` path. Work is fenced by reconciliation run and desired generation.
- Active-set observation uses the complete paginated `GET /api/v1/meetings?status=in_progress` result every five seconds and immediately after commands.
- If desired is absent, send `goto_meeting`, then OBS-targeted `goto_meeting_embed`, before Start or Resume. Goto failure does not prevent attempting the desired status transition.
- Start new before Pause old. Existing active meetings are preserved until desired is observed `in_progress`; cleanup failures report `MultipleInProgress / Degraded` and are retried.
- Desired `ready` and `paused` meetings receive Start. Desired `completed` meetings may be automatically Resumed once per generation.
- Automatic Resume consumption is persisted before dispatch with generation and completed `updated_at`. A crash, timeout, or unknown result cannot repeat it. Re-arm requires a dedicated generation increment.
- A consumed generation that is still `completed` remains `Blocked / ResumeLimitReached` and continues observation without another Resume.
- Normal Stop preflights current active meetings, rejects new ordinary work, and repeatedly Pauses all `in_progress` meetings until a fresh observation is empty.
- A room whose active set cannot be observed cannot normally Stop. Force-stop remains available.
- Force-stop is exposed immediately during stopping and runs automatically after 30 seconds. It invalidates local old-run work, permits immediate restart, and reports `RemoteOutcomeUnknown` because accepted Rozeta commands cannot be revoked.
- All Rozeta requests share a six-request scheduler with control-request priority. Start, Pause, and Resume dispatch at most once per observation round.
- WebSocket and HTTP snapshots carry a process epoch because room revisions restart from zero.
- Persistent state supports one process replica. Do not deploy multiple replicas without leader election.
