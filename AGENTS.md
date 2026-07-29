# AGENTS

## Overview

- `go run .` starts the only backend process: Gin HTTP, websocket, and the embedded admin UI.
- `main.go` wires the server, `state.go` owns room state, and `ws.go` owns websocket session handling.
- `rozeta-command-panel.user.js` is the room-side userscript; `token.js` only sets the Rozeta `auth_token` cookie.

## Run

- Open the admin UI at `https://coscup.1li.tw` after `go run .`.
- Load `rozeta-command-panel.user.js` into Tampermonkey on each always-on room browser.
- Set the same backend URL and room name in the userscript panel.

## Commands

- `go test ./...` runs the Go tests.
- `go test ./... -run TestAgentHelloRejectsDuplicateRoom` is the focused websocket ownership check.

## Runtime Facts

- Agents connect on `/ws/agent`; admins connect on `/ws/admin`.
- Heartbeats are sent every 1 second, and rooms are marked lost after 3 seconds without one.
- Room claims are exclusive: the server rejects a second agent for the same room with websocket close code `4409`.
- Commands are broadcast to all agents, but each agent only executes messages for its own room name.
- Supported commands are `goto`, `start`, and `pause`.
- `meeting-names.json` in the repo root is optional and maps meeting IDs to display names.
- Use `goto` first, then `start` after the room finishes loading.
