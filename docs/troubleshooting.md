# Troubleshooting

## CAPTCHA during login

On first login, or after Proton grows suspicious of a login attempt, the API replies
with error 9001 (human verification required) instead of logging you in. `login`
catches this and walks you through it:

```
proton-drive-fs login
```

1. Proton lists the verification methods it offers (email, sms, captcha).
2. `login` tries email, then sms, then captcha, unless you forced one with
   `-hv-method`.
3. For a CAPTCHA, the CLI prints the verify.proton.me URL and opens it in a browser
   unless `-no-browser` is set, in which case open the URL yourself.
4. Solve the CAPTCHA in the browser, then press Enter in the terminal to continue.

## Account temporarily locked

Proton's API can return error 2028 (account temporarily locked) after several failed
login attempts in a row. This is enforced on Proton's side, not something
proton-drive-fs can bypass. Wait before retrying; repeated retries while locked only
extend the wait.

## "Device or resource busy" on unmount

```
proton-drive-fs unmount ~/ProtonDrive
```

A plain unmount fails as busy while a process still has a file or the mountpoint open.
`unmount` retries automatically for `-wait` (default 5s), then falls back to a lazy
unmount that detaches the mount immediately and prints the processes still holding it
open; the kernel drops the mount once those processes let go.

If the daemon has died or deadlocked and programs are stuck on the mount instead of
just holding it open, use:

```
proton-drive-fs unmount -force ~/ProtonDrive
```

This lazily unmounts and aborts the kernel-side FUSE connection, so anything blocked
on the mount gets an error instead of hanging. It needs no root for a mount you own.

## Stale daemon after a rebuild

If an earlier unmount failed as busy, the old daemon can keep serving a mountpoint
after you rebuild the binary. `mount` guards against this: it refuses to attach to a
mountpoint that is already mounted and prints the running daemon's pid and version, so
a rebuild is never mistaken for actually replacing what is running. Check what is
actually running with:

```
proton-drive-fs status ~/ProtonDrive
```

`status` reports a version mismatch between the running daemon and the current binary
and prints the exact unmount-then-mount command to fix it. `make restart` does the
same in one step:

```
make restart
```

## Logs

The mount daemon logs structured entries to the systemd user journal under the
identifier `proton-drive-fs`, asynchronously so a slow or backed-up log write never
stalls a filesystem operation.

Two levels matter day to day: `info` covers one line per event (mounting and
unmounting, opening a file, uploading, creating, renaming, moving, deleting, a remote
change applied, pause and resume, login and logout); `debug` adds the technical
fields behind those (block-level cache hits and misses, API call timing, listing page
counts, keyring unlocks, and more). `-log-level` on `mount` sets which of `debug`,
`info`, `warn`, or `error` gets logged (default `info`). Nothing here ever logs a
token, password, key material, or file content.

Read the log with:

```
journalctl --user -t proton-drive-fs -f
journalctl --user -t proton-drive-fs -p debug -f
journalctl --user -t proton-drive-fs -o verbose -n 20
```

`-p debug` follows at debug level and above, picking up the technical fields.
`-o verbose` shows every field on a log line (path, size, elapsed, err, and so on) as
a journal field, uppercased with dots turned into underscores (`op` becomes `OP`,
`cache.hit` becomes `CACHE_HIT`); the plain `journalctl` view hides them.

Without a systemd journal to write to, or with `-log-stderr`, the daemon falls back to
plain text on stderr, which for a detached mount goes to
`$XDG_STATE_HOME/proton-drive-fs/mount.log` (falling back to
`~/.local/state/proton-drive-fs/mount.log`). `-log-stderr` forces this fallback even
when the journal is available, which is handy with `-foreground` at a terminal.

## Where things live

| What | Path |
|---|---|
| Session (tokens, username, key password fallback) | `$XDG_CONFIG_HOME/proton-drive-fs/session.json` |
| Tray's remembered mountpoint | `$XDG_CONFIG_HOME/proton-drive-fs/tray.json` |
| On-disk block cache | `$XDG_CACHE_HOME/proton-drive-fs/blocks` (`-cache-dir`) |
| Thumbnails | `$XDG_CACHE_HOME/thumbnails` (`-thumbnail-dir`) |
| Mount log (no journal, or `-log-stderr`) | `$XDG_STATE_HOME/proton-drive-fs/mount.log` |
| Status snapshot (pid, version, transfers) | `$XDG_RUNTIME_DIR/proton-drive-fs/status.json` |
| Pause marker | `$XDG_RUNTIME_DIR/proton-drive-fs/paused` |

Every `$XDG_*_HOME` path falls back to the matching directory under `$HOME` (for
example `~/.config`, `~/.cache`, `~/.local/state`) when the environment variable is
unset.
