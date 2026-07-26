# AGENTS

## Overview

This repository documents a remote management flow for Rozeta meeting rooms.

The main pieces are:

- `rozeta-command-panel.user.js` for browser automation
- backend websocket and room-state services
- admin UI for operators

## How to Start

1. Run `go run .`.
2. Open `http://localhost:8080` and verify the admin UI loads.
3. Load `rozeta-command-panel.user.js` into Tampermonkey.
4. Open a Rozeta room page in an always-on browser.
5. Set the backend URL and room name in the userscript panel.

## How to Debug

- Check the userscript log panel in the top-right corner.
- Check browser console output for DOM matching logs.
- Verify heartbeat arrival every 1 second.
- Confirm the backend marks a room lost after 3 seconds without heartbeat.
- Verify command routing by room name and current meeting id.
- Watch the admin alert strip for heartbeat-loss events.

## Notes

- Room state is updated from heartbeat snapshots.
- Commands are broadcast, but agents only execute matching room commands.
- `goto_and_start` is the preferred combined flow for switching and resuming.
