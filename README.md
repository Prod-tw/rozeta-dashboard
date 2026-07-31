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

## Container

Build and run the container with the token CSV mounted read-only. Credentials remain runtime environment variables and are not stored in the image:

```sh
docker build -t image.prod.tw/rozeta-dashboard:latest .
docker run --rm -p 8080:8080 \
  -e ADMIN_PASSWORD \
  -e SESSION_SECRET \
  --mount type=bind,src="$(pwd)/room.csv",dst=/data/room.csv,readonly \
  image.prod.tw/rozeta-dashboard:latest
```

`.github/workflows/container.yml` tests every pull request and builds the image without publishing it. Pushes to `main`, version tags matching `v*`, and manual workflow runs publish `image.prod.tw/rozeta-dashboard` for `linux/amd64` and `linux/arm64`. Configure the registry password as the GitHub Actions repository secret `REGISTRY_PASSWORD`; the workflow injects it only into `docker/login-action` and logs in as `prod`.

## License

MIT
