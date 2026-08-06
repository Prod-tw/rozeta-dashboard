# Design

## Overview

The service is a declarative controller for Rozeta room accounts. Each account token is exclusively owned by its room controller, which may Start, Resume, and Pause any meeting returned for that account. Persisted desired state identifies a meeting and generation; Rozeta responses are observations, never desired state.

Reconciliation lifecycle is process-local and is never persisted. Its states are `suspended`, `starting`, `active`, and `stopping`. Every process start leaves every room `suspended / ActiveSetUnknown` until an administrator starts it. Active means the controller is maintaining the room invariant, not that the remote desired meeting has already reached `in_progress`. The virtual desired meeting `__controller_preparation__` is an explicit schedule origin, not a lifecycle state.

## Desired State and Persistence

`-state` selects a versioned JSON file, defaulting to `controller-state.json`. Version 3 persists each room's `meeting_id`, `generation`, schedule offset in minutes, and consumed automatic-Resume record. The consumption record includes the generation and completed meeting `updated_at` that authorised the destructive operation. Lifecycle, remote observations, and client-only alert settings are not persisted. This is a breaking state format: older versions are rejected and must be recreated manually.

Writes use a synced temporary file and atomic rename. A desired update is not visible in memory unless persistence succeeds. A malformed, unsupported, or invalid existing file stops startup. Older state versions are rejected without migration; operators must recreate the v3 state file before startup.

The startup-loaded OPASS/session schedule is authoritative for meeting ordering. `-session` is required, and only meetings present in OPASS, the session mapping, and Rozeta are retained; the controller then prepends the virtual `準備` meeting with no API time, while internal next-meeting ordering uses a deliberately fake earliest start and no OPASS mapping. Cross-source unmatched records are ignored and logged; duplicate IDs, malformed data, invalid retained starts, and remote failures activate a server-wide `503` diagnostic gate. Retained starts must be unique within each room. The complete paginated Rozeta meeting identity intersection is captured at startup; later reads may change status, while unmatched additions or removals are ignored and logged. OPASS is never reloaded. A room without persisted desired state reports `InitialMeetingRequired`. A suspended room may persist a desired meeting or re-arm a generation without remote commands. An active desired update increments generation and reconciles immediately; selecting a completed desired meeting requires destructive preflight and confirmation before the new generation is accepted.

Re-sending the same desired meeting does not implicitly re-arm automatic Resume. A dedicated destructive re-arm operation increments generation for that same meeting and grants the new generation one allowance.

## Active-Set Reconciliation

The authoritative running observation is the complete paginated result of `GET /api/v1/meetings?status=in_progress` for the room account. While active with a real desired meeting, the eventual invariant is:

```text
active meeting IDs == {desired meeting ID}
```

Convergence is reported only after a fresh active-set observation proves this equality. Each room uses one serial actor and one coalesced `Observe -> Diff -> Act -> Requeue` path. Events, timers, requests, results, and completions are fenced by reconciliation run and desired generation. The active set is polled every five seconds and immediately after control commands.

When desired is `__controller_preparation__`, any successful active-set observation is converged and the normal reconcile path dispatches no Goto, Start, Resume, or cleanup Pause. Explicit Stop still pauses every observed active meeting. `advance-and-start` is the only transition from the preparation origin to the first real scheduled meeting.

Reconciliation is availability-first and eventually unique. If the desired meeting is absent from the active set, the controller attempts the complete ordered Goto pair, then reconciles the desired status even if Goto failed or its outcome is unknown. Goto sends `goto_meeting` first and OBS-targeted `goto_meeting_embed` second. A subsequent round repeats Goto before Start or Resume while desired remains absent. Once desired is observed `in_progress`, its route is treated as correct and Goto stops.

Desired meeting transitions are:

- `ready` or `paused`: send Start.
- `in_progress`: send no Start.
- `completed`: automatically Resume at most once for the desired generation, then Start as required.

Automatic Resume consumption is persisted atomically before dispatch. A crash, timeout, or unknown result therefore cannot cause another destructive Resume in the same generation. Start confirmation grants the allowance for the current generation. If that generation later remains or returns `completed` after consumption, the room stays `active / Blocked / ResumeLimitReached`, continues observing, and does not Resume again.

Only after desired is observed `in_progress` does the controller Pause every other active meeting. A failed cleanup Pause leaves desired available, reports `MultipleInProgress / Degraded`, and is retried after observation. If desired is missing, inaccessible, or cannot be started, existing active meetings are preserved; only explicit Stop targets them for Pause.

Start, Pause, and Resume are observation-driven. At most one command is dispatched per reconcile round, followed by a fresh observation before another dispatch. Resume is never blindly retried within an HTTP retry loop. All Rozeta requests share a six-request scheduler with control-request priority.

## Start and Preflight

Start preflight reads each frozen target's desired status and complete active set. It exposes completed meetings and destructive Resume risk before confirmation. Start and active desired-change confirmation explicitly warn that Resume permanently deletes completed transcripts and translations.

Start All preflight may partially succeed: rooms whose remote state cannot be observed remain suspended, while observable rooms may enter `starting` and then `active`. Before remote preflight, the request atomically validates the process epoch, unique valid room names, and every expected reconciliation run. The browser uses the room list frozen when confirmation opened; omitted rooms are unaffected.

## Stop and Force-Stop

Normal Stop is a declarative transition to an empty active set. Its preflight observes and displays every current `in_progress` meeting. If the active set cannot be observed, normal Stop is not accepted; the administrator may retry or force-stop.

Once accepted, Stop immediately disables automatic Resume and rejects new Goto, Start, Resume, desired-update, and ordinary reconcile work. One HTTP request dispatched before acceptance may finish or time out, but no later step from that old reconcile sequence may begin. The stopping loop repeatedly observes the active set and Pauses every active meeting. It becomes suspended only after a fresh observation proves the set is empty.

A stopping room exposes Force-stop immediately and automatically force-stops after 30 seconds. Force-stop invalidates and ignores local old-run work but cannot revoke a command already accepted by Rozeta. It transitions immediately to `suspended / RemoteOutcomeUnknown` and permits a new Start. A successful normal Stop retains `LastStopConfirmedEmpty` as a stale historical observation; suspension never claims the remote active set is still empty.

## Operations and UI

All per-room and bulk Start, Stop, and Force-stop actions require confirmation. Per-room actions require the expected reconciliation run. Bulk actions atomically validate process epoch, unique room names, and all expected runs before changing any room. A conflict changes no rooms and returns the authoritative snapshot.

Snapshots expose lifecycle, reconciliation run, desired meeting and generation, persisted schedule offset, desired status, active meeting IDs, active-set observation time and staleness, recent actions, errors, and structured conditions. Core conditions are `ReconciliationActive`, `DesiredMeetingSoleInProgress`, `ActiveSetObserved`, and the latest Goto dispatch result. Schedule timing alerts are intentionally client-only: each browser may enable alerts globally, choose thresholds per selected room, and use the persisted room offset to compare both the next schedule start and whether the current desired meeting has entered too early. A random process epoch separates revision counters across restarts; authoritative full snapshots remove absent rooms and meeting caches, and clients ignore incremental snapshots from an old epoch.

Summary states include `Converged / DesiredMeetingSoleInProgress`, `Converged / PreparationMeeting`, `Reconciling / StartingDesiredMeeting`, `Degraded / MultipleInProgress`, `Blocked / DesiredMeetingMissing`, `Blocked / ResumeLimitReached`, `Reconciling / PausingAllMeetings`, `Suspended / ActiveSetUnknown`, `Suspended / LastStopConfirmedEmpty`, and `Suspended / RemoteOutcomeUnknown`.

The current persistence design supports one controller process. Multiple replicas require leader election.
