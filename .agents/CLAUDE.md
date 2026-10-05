## Layout

- `cmd/proton-drive-fs`: the CLI (`main.go` subcommands, `config.go` flag registration and `config` subcommand, `autostart.go`, `tray.go`) and the embedded systemd unit template `proton-drive-fs.service`.
- `internal/auth`: login (SRP, 2FA, human verification) and session storage (file plus OS keyring).
- `internal/drive`: Proton Drive API client: listings, block download and cache, upload, move, trash, events, crypto.
- `internal/fusefs`: the FUSE layer (go-fuse): mount, directory and file nodes, write buffers, thumbnails (`previews.go`), status publishing.
- `internal/config`: TOML config file, built-in defaults, flag-to-config mapping, `config init` output.
- `internal/api`: the daemon's local API on a unix socket (status, transfers, cache, pause, logs).
- `internal/state`: runtime files shared by the mount, the tray and the local API (pause marker, status snapshot).
- `internal/tray`: system tray icon and menu.
- `internal/thumbs`: freedesktop thumbnail cache writer.
- `internal/logx`: process-wide `slog` setup (journald or stderr).
- `internal/about`: the About dialog text and bundled third-party licenses.

## Commands

- `make build`: static binary `./proton-drive-fs` in the repo root.
- `make test`: `go test ./...`.
- `make race`: `go test -race ./...`.
- `make lint`: `gofmt -l`, `go vet ./...` and golangci-lint.
- `make check`: `test` then `lint`.
- `make generate`: regenerates tray icons and bundled licenses.
- `make install`: builds, then copies the binary to `$(PREFIX)/bin` (`PREFIX` defaults to `$HOME/.local`), plus the desktop entry and icon.
- The systemd user unit comes from `proton-drive-fs autostart`, which renders `cmd/proton-drive-fs/proton-drive-fs.service` into `~/.config/systemd/user/proton-drive-fs.service`. Its `ExecStart` is the path of the binary that ran `autostart` (the template has `@BINDIR@/proton-drive-fs mount -foreground`).
- The running daemon is the installed binary, not `./proton-drive-fs` in the repo. After a rebuild, run `make install` and restart the user unit (`systemctl --user restart proton-drive-fs`) before the change takes effect.
- `make restart MP=<mountpoint>` unmounts and remounts the repo-root binary instead. It does not touch the installed binary or the unit.

## Adding a config option

Every place below changes together. `upload_delay` (a duration) and `upload_delays` (a list) were added this way.

- `internal/config/config.go`: add the field to `Config` with its `toml` tag, and set its default in `Defaults()` (`UploadDelay: "5s"`).
- `internal/config/flags.go`: add a row to the `fields` table, mapping the flag name to the TOML key and a setter (`{"upload-delay", "upload_delay", ...}`).
- `internal/config/init.go`: add the documented entry to `initFields`, used by `config init`. `Upgrade` (run by `LoadOrInit` on every command) appends new `initFields` keys to existing user files and drops commented-out defaults of removed ones, so removing a key from `initFields` is enough to retire it from users' files. A key the user set is never touched.
- `cmd/proton-drive-fs/config.go`: register the flag in `registerMountConfigFlags` with the config value as its default, add the pointer to `mountConfigFlags`, and add a `line(...)` in the `config show` output.
- `cmd/proton-drive-fs/main.go`: pass the value into `fusefs.Options` in the mount path, and add the flag to the usage string of `mount`. Add the matching field to `fusefs.Options` in `internal/fusefs/fusefs.go`.
- Docs: add a row to the tables in `docs/configuration.md` and `docs/usage.md`.

## Write path invariants

Code is in `internal/fusefs/fusefs.go`. Detail is in section 7 of `NOTAS-DE-ARQUITETURA.md`.

- A write buffers to a local temp file and uploads on `Release`. Writes are whole-file, with one writer per file.
- A released buffer can be parked (`fileHandle.park`). A name matching `exclude` parks with no timer. Any other name parks for its upload delay, then `settle` uploads it.
- A rename, a reopen for write, or an unlink takes the parked buffer back (`unpark`). `unpark` returns false when the buffer was no longer parked, so two paths never upload the same buffer.
- A rename on either path (`renamePending` for a new file, the regular `Rename` for an existing one) restarts a parked buffer's wait under the new name.
- The buffer stays on the node (`fn.handle`) until the upload succeeds, then `drop` removes it. A failed upload calls `keepForRetry`, which parks it again for `uploadRetryDelay`; a close has already returned success, so dropping it would lose the save.
- Readers of a buffered file get their own descriptor (`readerOf`), so they keep working after `drop` removes the temp file.
- A parked new file is not on the drive yet, so lookups and listings include pending children.
- Unmount flushes waiting buffers (`mountState.flushDelayed`) before the server unmounts.

## Conventions

- Comments are dense and explain why. Keep that style when editing nearby code.
- A `ponytail:` comment marks a deliberate simplification and states its limit. Add one when you cut scope on purpose, and keep existing ones accurate.
- Commit subjects follow Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`), with a single `-m` and no body. Subject types drive the automatic release.
- Work directly on `main`: no branches, no pull requests. `docs/contributing.md` still describes a branch and PR flow, which does not match this.
- Run `go vet ./...` and `go test ./...` before committing.
- When behavior changes, update the user docs under `docs/`, `README.md` and `NOTAS-DE-ARQUITETURA.md`.
- `NOTAS-DE-ARQUITETURA.md` is in Portuguese. It and `HANDOFF.md` are local-only, excluded through `.git/info/exclude`. Run `git status --ignored` before assuming either is tracked.
