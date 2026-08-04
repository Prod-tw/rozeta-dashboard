# Active Meeting Reconciliation Decisions

## Ownership And Desired State

- Every account token is exclusively owned by its corresponding room controller. The controller may Start, Resume, and Pause any meeting returned for that account.
- Persisted desired state contains only `meeting_id`, `generation`, and the consumed automatic-Resume record. `desired_running` is removed.
- A suspended room may update and persist its desired meeting without sending any remote command.
- An active desired update reconciles immediately. Switching to a completed meeting requires a destructive preflight and confirmation before accepting the new generation.
- A room without persisted desired state is `InitialMeetingRequired`. Session schedule data only recommends and orders meetings; it never automatically selects desired state.

## Lifecycle

- Reconciliation lifecycle is process-local and is never persisted.
- Lifecycle states are `suspended`, `starting`, `active`, and `stopping`.
- Every process start leaves every room `suspended / ActiveSetUnknown` until an administrator starts it.
- Active means the controller is maintaining the room invariant; it does not itself mean the remote meeting is already in progress.
- Per-room and browser-captured bulk Start, Stop, and Force-stop controls remain available.
- All Start, Stop, and Force-stop actions require confirmation.
- Per-room lifecycle actions require the expected reconciliation run. Bulk requests atomically validate process epoch, unique valid room names, and every expected run before changing any room.
- Browser-captured bulk operations use the frozen room list from the confirmation flow. Omitted rooms are unaffected.

## Active-Set Invariant

- The authoritative running observation is the complete paginated result of `GET /api/v1/meetings?status=in_progress` for the room account.
- While active, the eventual invariant is that the desired meeting is the account's only `in_progress` meeting.
- Convergence is reported only after a fresh active-set observation equals exactly `{desired}`.
- The design is availability-first and eventually unique: when switching meetings, Start the new desired meeting before pausing old active meetings.
- Once desired is observed `in_progress`, Pause all other active meetings. A failed old-meeting Pause leaves desired running, reports `MultipleInProgress / Degraded`, and continues retrying cleanup.
- If desired is missing, inaccessible, or cannot be started, preserve existing active meetings. Only an explicit Stop pauses them.
- If desired is already `in_progress`, treat browser route as correct, do not send Goto, and clean up any other active meetings.
- If desired is not `in_progress`, attempt the complete ordered Goto pair, then immediately reconcile desired to `in_progress` even if Goto failed or its outcome is unknown.
- If desired becomes `in_progress`, treat route as correct and stop retrying Goto. If desired remains absent from the active set, the next reconcile round attempts Goto again before Start or Resume.
- Goto is always sent as normal `goto_meeting` followed by OBS-targeted `goto_meeting_embed`.

## Meeting Transitions

- A desired meeting in `ready` or `paused` receives Start.
- A desired meeting in `in_progress` needs no Start.
- A desired meeting in `completed` may be automatically Resumed at most once per desired generation.
- Automatic Resume consumption is persisted atomically before dispatch, including generation and completed `updated_at`, so crash or timeout cannot repeat the destructive operation for that generation.
- Start confirmation authorises one automatic Resume for the current desired generation. Active switching to a completed desired meeting requires a fresh destructive confirmation for the new generation.
- If the same generation reaches `completed` after its Resume allowance was consumed, the room remains `active / Blocked / ResumeLimitReached` and continues observation without another Resume.
- A dedicated destructive re-arm operation increments generation for the same desired meeting and grants the new generation one Resume allowance. Re-sending the same desired update never implicitly re-arms it.
- Re-arm may update suspended desired state without remote commands; an active re-arm reconciles immediately.

## Stop And Force-Stop

- Normal Stop is a declarative transition whose remote target is an empty active set.
- Stop immediately disables automatic Resume and rejects new Goto, Start, Resume, desired-update, and ordinary reconcile work.
- A single HTTP request already dispatched when Stop is accepted may finish or time out. No later step from its old reconcile sequence may start.
- Stopping then repeatedly observes the active set, Pauses every `in_progress` meeting, and becomes suspended only after a fresh observation is empty.
- Stop preflight displays the current active meetings. A room whose active set cannot be observed is not normally stopped; the administrator may retry or force-stop it.
- A stopping room exposes Force-stop immediately and automatically force-stops after 30 seconds.
- Force-stop cancels and ignores local old-run work but cannot revoke commands already accepted by Rozeta. It becomes `suspended / RemoteOutcomeUnknown` and may restart immediately.
- A fresh normal-stop empty observation is retained as `LastStopConfirmedEmpty` but becomes stale after suspension. Suspended never claims the remote active set is currently empty.

## Observation And Controller Shape

- Online-count, `WaitingForClients`, presence readiness, `navigationReady`, and Goto grace timers are removed from control and admin UI.
- Each room uses one serial actor and one coalesced reconcile path: `Observe -> Diff -> Act -> Requeue`.
- Every event, timer, request, result, and completion is fenced by reconciliation run and desired generation.
- Reconcile polls the active set every five seconds and immediately after control commands. Full meeting data remains available for selection and desired-status checks.
- Start, Pause, and Resume retries are observation-driven: dispatch at most once per reconcile round, then perform a fresh observation before another dispatch. Resume is never blindly retried inside one HTTP retry loop.
- All Rozeta requests continue through the global six-request scheduler with control-request priority.

## Conditions And Admin UI

- Snapshots expose lifecycle, reconciliation run, active meeting IDs, active-set observation time and staleness, desired status, recent actions, and structured conditions.
- Core conditions are `ReconciliationActive`, `DesiredMeetingSoleInProgress`, `ActiveSetObserved`, and the latest Goto dispatch result.
- Summary states include `Converged / DesiredMeetingSoleInProgress`, `Reconciling / StartingDesiredMeeting`, `Degraded / MultipleInProgress`, `Blocked / DesiredMeetingMissing`, `Blocked / ResumeLimitReached`, `Reconciling / PausingAllMeetings`, `Suspended / ActiveSetUnknown`, `Suspended / LastStopConfirmedEmpty`, and `Suspended / RemoteOutcomeUnknown`.
- Start preflight reads each frozen target's desired status and active set. It lists completed rooms and destructive risk before confirmation.
- Start All preflight may partially succeed. Rooms whose remote status cannot be observed remain suspended; observable rooms may start. Optimistic epoch/name/run validation remains atomic before remote preflight.
- Start and active desired-change confirmation explicitly warn that automatic Resume permanently deletes completed transcripts and translations.
- Stop confirmation lists the active meetings that will be Paused.

## Persistence Migration

- Controller state version 2 removes `running` and persists consumed automatic-Resume information.
- Existing version 1 state is migrated atomically by preserving meeting ID and generation and dropping `running`.
- Migration never starts reconciliation or sends a remote command.

## Room Visibility And Bulk Scope

- The room picker controls only which rooms are displayed in the current browser. It does not change server state.
- Room visibility is stored in browser-local `localStorage`; different browsers and administrator devices have independent preferences.
- The first use and newly configured rooms default to visible. Only explicitly hidden rooms remain hidden.
- Search is case-insensitive plain substring matching. Character-pattern matching such as `?` and `*` is not supported.
- Search changes only the picker results. Visibility changes are applied only after an explicit picker action and `套用`; cancelling discards the draft.
- Hiding the currently selected room clears the selected room and its desired-state editor.
- `開始全部`, `停止全部`, and `強制停止全部` operate only on rooms visible when the batch button is pressed. The target list is frozen through preflight and confirmation.
- Batch lifecycle eligibility remains unchanged: Start targets visible `suspended` rooms, Stop targets visible `starting` or `active` rooms, and Force-stop targets visible `stopping` rooms.
- Hidden rooms are omitted from all batch requests. If no visible room is eligible, the corresponding batch button is disabled and sends no request.
