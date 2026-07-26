# Rozeta Remote Management

Remote control for multiple Rozeta meeting rooms using a browser userscript and a backend control plane.

## How to Run

1. Run the backend with `go run .`.
2. Open `http://localhost:8080` for the admin UI.
3. Install `rozeta-command-panel.user.js` in Tampermonkey on each always-on Rozeta browser.
4. Set the same backend URL and the room name in the userscript panel.
5. Use the admin UI to send `goto`, `start`, `pause`, or `goto_and_start`.
6. Optionally add a `meeting-names.json` file in the project root to map meeting IDs to display names.

## License

MIT
