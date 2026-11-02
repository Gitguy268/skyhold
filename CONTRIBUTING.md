# Contributing

Thanks for helping with Skyhold.

By taking part you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Reporting a bug or asking for a feature

Open an [issue](https://github.com/Gitguy268/skyhold/issues/new/choose) and use the template. For a bug, say which interface you used (CLI, TUI or web UI), the rclone backend type, your OS, what you ran and what happened. Never paste your cloud key, release password or remote connection string.

Report security problems privately: see [SECURITY.md](SECURITY.md).

## Making a change

1. Fork the repository and create a branch from `main`.
2. Build and run it. You need Go 1.27 or newer. The first build downloads every rclone backend and takes a few minutes.

   ```sh
   go build -o skyhold ./cmd/skyhold
   export SKYHOLD_HOME=/tmp/skyhold-dev SKYHOLD_KEYRING=file
   ./skyhold init
   ./skyhold remote add disk /tmp/skyhold-dev-cloud
   ```

   `SKYHOLD_HOME` and `SKYHOLD_KEYRING=file` keep a test run away from your real home folder and OS keychain. A local folder works as a cloud. **Only add scratch files**: `release` really deletes the originals.

3. Make your change. Keep commits small, each with a clear message.
4. Run the checks below and update the README if behaviour changed.
5. Open a pull request and fill in the template.

## Checks

```sh
gofmt -l .          # prints nothing
go vet ./...
go test -race ./...
```

The tests in `internal/core` run the whole cycle (add, push, verify, release, restore, recover) against a local folder as the cloud, with and without cloud encryption. To check a Windows or Linux build from another OS, run `GOOS=windows go vet ./...` and `GOOS=linux go vet ./...`.

## Rules for changes that delete things

- `release` must refuse unless every staged file is verified on every cloud at that moment.
- An original must be hashed again just before it is deleted, and kept if it changed.
- Nothing may be deleted after a wrong password, and no path may wipe anything automatically.
- Add a test in `internal/core` for any change to these paths.
- Never change the `skyhold-salt:` string in `internal/core/remote.go`, or every existing encrypted backup becomes unreadable.

## Dependencies

New dependencies need a licence compatible with MIT and a clear reason. Name them in the pull request and in [NOTICE](NOTICE). rclone backends are listed one by one in `internal/core/backends.go`. When upgrading rclone, compare that file with rclone's `backend/all/all.go`, and keep `smb` out: its dependency is LGPL-3.0.

To list the licences of everything compiled in:

```sh
go install github.com/google/go-licenses/v2@v2.0.1
GOROOT=$(go env GOROOT) go-licenses report ./cmd/skyhold
```

## Layout

| Package | What it does |
|---|---|
| `internal/core` | everything Skyhold does: vault, manifest, keyring, push, verify, release, restore, recover, disk-encryption checks. No UI code. |
| `internal/tui` | the terminal UI (Bubble Tea) |
| `internal/web` | the web UI: an embedded static page and a JSON API with a token |
| `cmd/skyhold` | the CLI (Cobra), which wires the three together |
