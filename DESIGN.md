# Design

## Overview

This system manages 24 Rozeta rooms remotely without the admin touching the Rozeta UI directly.

Each room has:

- one always-on browser session
- one userscript agent
- one user-defined room name
- one Rozeta account token

The backend provides:

- websocket broadcast for commands
- room state tracking
- heartbeat monitoring
- admin UI data

## Control Flow

1. Agent connects to the websocket broadcast channel.
2. Agent identifies itself with its room name.
3. Server broadcasts admin commands to all agents.
4. Each agent executes only commands matching its own room name.
5. Agent reports room snapshot data in heartbeat messages.
6. Server updates room state from the latest heartbeat.

## Heartbeat

- Sent every 1 second.
- Includes full room snapshot.
- Fields:
  - `room_name`
  - `agent_id`
  - `status`
  - `current_meeting_id`
  - `timestamp`
  - `last_command_id`
  - `last_command_result`
  - `last_error`

If the server does not receive a heartbeat for 3 seconds, it marks the room as lost and alerts the admin.

## Command Model

Commands are fire-and-forget.

- No per-command ack.
- The agent executes the command locally.
- Status is reported later through heartbeat.
- A newer command overwrites any pending command for the same room.
- Commands are treated as idempotent per `command_id`.

Supported actions:

- `goto`
- `start`
- `pause`
- `goto_and_start`

## DOM Execution

The userscript uses direct URL navigation and DOM interaction.

- `goto` changes the meeting URL.
- `start` clicks the play/start control.
- `pause` clicks the pause control.
- `goto_and_start` waits for the new room DOM and then starts.

## Admin UI

The admin page shows:

- room name
- current meeting
- room status

The admin can select a room and send commands to switch meetings.

## Design Considerations

- Broadcast keeps the server simple at the current scale.
- Client-side room filtering is acceptable because the number of agents is small.
- Heartbeat is the source of truth for live room state.
- The design assumes each room remains on a foreground browser tab.
- Duplicate agent handling is intentionally out of scope for v1.
