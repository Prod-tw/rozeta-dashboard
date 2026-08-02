# AGENTS

## Overview

- `go run . -account account.csv` starts the only process: Gin HTTP, admin WebSocket, embedded UI, and Rozeta synchronization.
- `main.go` wires routes and command execution, `rozeta.go` owns Rozeta HTTP calls, `state.go` owns room state, `auth.go` owns admin sessions, and `ws.go` owns admin WebSocket clients.
- There is no room userscript or agent WebSocket. Room state comes from Rozeta meeting APIs.

## Start

```sh
export ADMIN_PASSWORD='replace-with-a-strong-password'
export SESSION_SECRET='replace-with-at-least-32-random-bytes'
go run . -account account.csv
```

The token CSV is required. Missing files, malformed rows, empty fields, and duplicate room names stop startup. One token that Rozeta later rejects marks only that room as `authentication_error`.

## Debug

- `go test ./...` runs all tests.
- `go test -race ./...` checks concurrent room synchronization, command completion, and WebSocket state.
- Inspect the room's API status, last sync, last command result, and last error in the admin UI.
- Use Goto before Start or Pause if the server cannot resolve one current meeting.
- Start and Pause poll every 500 milliseconds for up to 15 seconds; later matches become `confirmed_late`.

## Runtime Facts

- Rooms synchronize every 10 seconds with a concurrency limit of six and a two-second deadline per room.
- Commands are serialized per room but different rooms can run commands concurrently.
- Goto completes when Rozeta accepts the command and cannot confirm browser navigation.
- Resume permanently deletes the completed meeting's transcription and translation data.
- Admin sessions have a fixed 72-hour lifetime and require HTTPS for the secure cookie.
- Runtime room and command state is not persisted across backend restarts.
