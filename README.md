# Rozeta Remote Management

Authenticated control panel for managing multiple Rozeta room accounts through the Rozeta meeting and command APIs.

## How to Run

Use Go 1.25 and provide the admin credentials plus the required room token CSV:

```sh
export ADMIN_PASSWORD='replace-with-a-strong-password'
export SESSION_SECRET='replace-with-at-least-32-random-bytes'
go run . -token-file room.csv
```

Open `http://localhost:8080` directly for development, or the configured HTTPS deployment URL in production. The secure session cookie requires HTTPS in production.

The CSV must start with either `account,User ID,Token` or `帳號,User ID,Token`. Room names are derived by removing `@coscup.org` from the account field.

Use Goto before Start or Pause when no unique active meeting can be resolved. Start and Pause wait up to 15 seconds for Rozeta to report the expected meeting status. Resume permanently deletes the selected completed meeting's transcriptions and translations before resetting it to ready.

Run tests with:

```sh
go test ./...
```

## License

MIT
