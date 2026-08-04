# Rozeta Declarative Controller

Authenticated control plane that lets administrators reconcile each Rozeta room account to one persisted desired meeting. While a room is active, the controller maintains the invariant that the desired meeting is the account's only meeting with `status=in_progress`.

## How to Run

Use Go 1.25 and provide the required account and session CSV files. Startup loads `opass.json` once, keeps only meetings present in OPASS, the session mappings, and Rozeta, and fixes each room's meeting order by its OPASS start time. Cross-source unmatched meetings are ignored and logged; malformed data, duplicate IDs, or remote failures leave the server available only as a `503` diagnostic page.

```sh
export ADMIN_PASSWORD='replace-with-a-strong-password'
export SESSION_SECRET='replace-with-at-least-32-random-bytes'
export EXTERNAL_API_TOKEN='replace-with-a-long-machine-token'
go run . -account account.csv -session session.csv -state controller-state.json
```

The account CSV starts with `account,User ID,Token` or `帳號,User ID,Token`. Configure OBS with Rozeta's authenticated embed URL, including `client=obs`; when navigation is needed, the controller sends normal `goto_meeting` followed by OBS-targeted `goto_meeting_embed`.

The versioned state file is authoritative and must be retained across restarts. Desired state contains the meeting ID, generation, and persisted automatic-Resume consumption record; lifecycle and run intent are process-local. An existing version 1 state file is atomically migrated to version 2 by preserving meeting IDs and generations and dropping `running`. Migration never starts reconciliation or sends a Rozeta command. A malformed or unsupported state file stops startup.

Every process start leaves every room `suspended / ActiveSetUnknown`. A room without persisted desired state remains `InitialMeetingRequired` until an administrator chooses one. Administrators explicitly start, stop, or force-stop rooms individually or through browser-captured bulk controls; every lifecycle action requires confirmation. OPASS is not reloaded after startup; later Rozeta status reads ignore and log meetings outside the validated snapshot while preserving the OPASS ordering.

The external `POST /api/v1/rooms/{room_name}/actions/advance-and-start` endpoint requires `Authorization: Bearer $EXTERNAL_API_TOKEN`. It advances only through meetings with a loaded `scheduled_start`, performs bounded remote preflight retries, and returns `503` without changing desired state if preflight cannot complete. The admin page keeps the failure visible as a room alert.

Start performs a remote preflight before activating a room. While active, the controller observes the complete paginated `status=in_progress` set every five seconds. It starts the desired meeting before pausing old active meetings, preserving availability while eventually converging to exactly `{desired}`. A completed desired meeting may be automatically Resumed at most once per generation; consumption is persisted before dispatch so a crash or timeout cannot repeat the destructive operation. Start and completed-meeting confirmations must warn that Resume permanently deletes completed transcripts and translations.

Normal Stop targets an empty active set. Its preflight lists the currently active meetings, then stopping repeatedly observes and Pauses every `in_progress` meeting until a fresh observation is empty. Force-stop is available immediately, runs automatically after 30 seconds, cancels local old-run work, and leaves remote outcome unknown because Rozeta may still apply an already accepted command.

Run the tests with:

```sh
go test ./...
go test -race -shuffle=on ./...
pnpm test
```

## Container

`compose.yaml` mounts account/session CSV files read-only and stores controller state in the `controller-state` named volume:

```sh
export ADMIN_PASSWORD='replace-with-a-strong-password'
export SESSION_SECRET='replace-with-at-least-32-random-bytes'
export EXTERNAL_API_TOKEN='replace-with-a-long-machine-token'
docker compose up -d
```

Use `docker compose down` to retain desired state, or `docker compose down -v` to remove it deliberately.

## License

MIT
