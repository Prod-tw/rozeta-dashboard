# Design

## Overview

The service manages Rozeta room accounts without browser automation. A single Go process serves the authenticated admin UI, calls Rozeta APIs with room-scoped tokens, tracks command state, and pushes updates to admin browsers over WebSocket.

## Control Flow

1. The server strictly loads rooms and tokens from the required CSV.
2. When `-session` is provided, the server joins its Rozeta meeting IDs to one startup snapshot of the COSCUP opass schedule.
3. Each room starts in `syncing` while the server loads its Rozeta meetings.
4. The server refreshes all rooms every 10 seconds with at most six concurrent requests and a two-second deadline per room.
5. The admin submits Goto, Start, Pause, or Resume for one room.
6. The server permits only one pending command per room and returns a command ID.
7. Admin WebSocket snapshots preserve loading state across page reloads and reconnects.

## Meeting Schedule

The optional session CSV maps `議程 ID` (Rozeta meeting ID) to `Session ID` (opass session ID). The server downloads `https://coscup.org/2026/api/opass.json` once at startup and retries every fetch, HTTP, or JSON failure up to three total attempts with ten-second request timeouts and one- then two-second delays.

The room meetings API is the only sorting authority. Known meetings sort by RFC 3339 start time, then Unicode title and meeting ID. Unknown meetings follow all known meetings while retaining their relative Rozeta order. Without `-session`, all meetings sort by Unicode title and meeting ID. The API includes `schedule_enabled` and optional `scheduled_start`; the admin formats known starts in `Asia/Taipei` as `MM/DD HH:mm` and labels unmatched meetings `未排程`.

Duplicate meeting IDs or session IDs in the CSV and duplicate opass session IDs stop startup because they make the join ambiguous. Empty CSV IDs, missing opass sessions, and invalid start times are logged individually and summarized, then degrade those meetings to unscheduled. Titles and rooms are deliberately not compared because IDs are authoritative and schedule edits can leave descriptive CSV fields stale.

## Current Meeting

A successful Goto target is authoritative for the rest of that server process. Without a Goto target, synchronization selects a unique `in_progress` meeting, then a unique `paused` meeting. Multiple matching meetings or only ready meetings leave the current meeting unresolved, so Start and Pause require Goto first.

This is meeting state, not browser presence. Removing the userscript means the service cannot determine whether a room browser is online or confirm that Goto changed its route.

## Commands

- Goto calls `POST /api/v1/commands` with `goto_meeting` and completes when Rozeta accepts the request.
- Start and Pause call `start_meeting` or `pause_meeting`, then query the target every 500 milliseconds.
- A matching meeting status is success even if the command request itself returned an error.
- Confirmation stops after 15 seconds. A later periodic match changes the result to `confirmed_late`.
- Network ambiguity never triggers an automatic command retry.
- Resume calls `POST /api/v1/meetings/{id}/resume` and confirms the resulting `ready` status.

Resume is destructive: Rozeta permanently deletes transcription and translation data. The UI requires a separate confirmation but does not combine Resume with Goto or Start.

## State

Room and command state is in memory. It contains the current meeting, Rozeta API health, last successful sync, last command result, and at most one pending command. Admin page reloads recover this state, but a backend restart intentionally discards command history and prior Goto targets.

Transient synchronization failures retain the last known meeting state and mark the room `stale`. Start and Pause are disabled while stale because their target cannot be resolved safely; Goto remains available because it has an explicit target. Authentication failures disable the room until a later synchronization succeeds.

## Authentication

`ADMIN_PASSWORD` and a `SESSION_SECRET` of at least 32 bytes are required at startup. A successful login receives an HMAC-signed, `HttpOnly`, `Secure`, `SameSite=Strict` cookie with a fixed 72-hour lifetime. The admin page, room APIs, command API, logout, and admin WebSocket all validate it.

Failed login attempts are limited in memory to ten per direct client IP in five minutes. Security headers prohibit framing, cross-origin form actions, inline scripts, and cross-origin API requests.

## Design Considerations

- Direct Rozeta commands avoid selectors and DOM changes that made browser automation fragile.
- API success for Goto is dispatch confirmation, not browser execution confirmation.
- Start and Pause use outcome-based confirmation because meeting status is more useful than command transport status.
- Per-room exclusion prevents overlapping commands while allowing different rooms to operate concurrently.
- Strict CSV parsing fails early instead of silently omitting a room or choosing between duplicate credentials.
- Schedule sorting is limited to API responses so it cannot change current-meeting inference or command behavior.
- The opass schedule is immutable for one process lifetime; operators restart the service to pick up event schedule changes.
- No database is added because runtime command history is operational rather than durable data.
