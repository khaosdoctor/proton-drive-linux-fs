# Usage

proton-drive-fs is one binary with eight subcommands: `login`, `mount`, `unmount`,
`status`, `tray`, `logout`, `version`, `config`.

Every flag `login`, `mount`, and `tray` accept also has a matching key in a config
file; see [Configuration](configuration.md) for the file location, its keys, and how a
flag and the file resolve together.

## login

```
proton-drive-fs login [-config path] [-no-browser] [-hv-method captcha|email|sms]
```

Prompts for username, password, and a TOTP code if two-factor is enabled.

- `-no-browser` (default: `false`): do not open a browser for human verification. On
  first login Proton may require it (CAPTCHA, email code, or SMS code); for a CAPTCHA
  the CLI prints the verify.proton.me URL and opens it unless this flag is set, in
  which case open the URL yourself.
- `-hv-method` (default: none forced): force a specific verification method
  (`captcha`, `email`, or `sms`). Without it Proton's offered methods are tried in the
  order email, sms, captcha.

A successful login writes a session file to
`$XDG_CONFIG_HOME/proton-drive-fs/session.json` (falls back to
`~/.config/proton-drive-fs/session.json`), mode 0600 in a 0700 directory. The salted
key password derived from the account password unlocks the drive's encryption keys on
later runs; it goes to the OS keyring when one is available, and otherwise stays in the
session file with mode 0600. `logout` removes both.

## mount

```
proton-drive-fs mount [<mountpoint>] [-config path] [flags]
```

`<mountpoint>` is required unless the config file sets `mountpoint`. If it does not
exist, mount creates it. By default mount detaches into the
background and waits until the filesystem is mounted. The daemon logs structured
entries to the systemd journal itself under the identifier `proton-drive-fs`, readable
with `journalctl --user -t proton-drive-fs`; see
[Logs](troubleshooting.md#logs) for levels and the file fallback when there is no
journal.

| Flag | Default | Meaning |
|---|---|---|
| `-debug` | `false` | Enable FUSE debug logging. |
| `-ttl` | `30s` | How long a directory listing stays cached before it is fetched again. |
| `-poll` | `10s` | How often the event feed is polled for remote changes. |
| `-op-timeout` | `60s` | Deadline for one filesystem operation's network calls (listing, open, read, upload, mkdir, remove, rename). An operation stuck past this returns an error instead of hanging the caller. Uploads scale past this for large files. |
| `-cache-dir` | `$XDG_CACHE_HOME/proton-drive-fs` (falls back to `~/.cache/proton-drive-fs`) | Where downloaded, decrypted file blocks (under `blocks/`) and persisted directory listings (under `listings/`) are stored on disk so they survive a remount. |
| `-cache-size` | `2GiB` | Total size the on-disk cache is allowed to use, shared by blocks and persisted listings together. Accepts suffixes like `512MiB` or `2GiB`. A value of 0 or less disables the on-disk cache, both kinds. |
| `-large-file` | `300MiB` | Files larger than this are still read lazily block by block, but their blocks are not stored in the on-disk cache, so one large file cannot evict everything else. 0 disables the threshold. |
| `-thumbnails` | `true` | Write the preview image Proton stores for a file into the freedesktop thumbnail cache when a folder is listed. |
| `-thumbnail-dir` | `$XDG_CACHE_HOME/thumbnails` (falls back to `~/.cache/thumbnails`) | The thumbnail cache directory to write into. This is the shared directory file managers read, not a directory of its own. |
| `-deny-readers` | see below | Comma-separated process names refused a read of a file above `-large-file`. Passing a value replaces the default list; `-deny-readers ""` turns the refusal off. |
| `-max-uploads` | `5` | How many files upload at once. The rest wait in line instead of opening a connection each. 0 or less removes the cap. |
| `-max-downloads` | `8` | How many file blocks download at once, across every open file. 0 or less removes the cap. |
| `-foreground` | `false` | Stay attached to the terminal instead of detaching into the background; used by the systemd unit. |
| `-log-level` | `info` | Log verbosity: `debug`, `info`, `warn`, or `error`. See [Logs](troubleshooting.md#logs). |
| `-log-stderr` | `false` | Force logging to stderr instead of the systemd journal; useful with `-foreground` at a terminal. |

The `-deny-readers` default list:

```
tracker-miner-fs, tracker-extract, localsearch, baloo_file, baloo_file_extractor,
tumblerd, ffmpegthumbnailer, totem-video-thumbnailer, gdk-pixbuf-thumbnailer,
gnome-desktop-thumbnailer, evince-thumbnailer
```

`mount` refuses to attach to a mountpoint that is already mounted, printing the running
daemon's pid and version when the status file has them, so a rebuild whose earlier
unmount failed as busy never gets mistaken for actually running the new binary.
`make restart` (optionally `MP=<mountpoint>`, default `~/ProtonDrive`) unmounts,
rebuilds, and remounts in one step.

A cold directory (nothing cached in memory yet, for example right after mount) is
served from the persisted listing cache when one exists, so a folder listed before
shows up instantly instead of waiting on the network; a background refresh follows the
same TTL and event-driven invalidation as everything else, and refreshes the persisted
copy once it lands. See [Cache layout](how-it-works.md#cache-layout) for how blocks
and listings share the cache directory.

## unmount

```
proton-drive-fs unmount [-force] [-wait 5s] <mountpoint>
```

Runs `fusermount3 -u` (or `fusermount -u` if `fusermount3` is not on `PATH`).

- `-wait` (default: `5s`): if the mountpoint is busy, retry every 500ms for up to this
  long. If it is still busy after that, unmount falls back to a lazy unmount, which
  detaches the mount right away and lets the kernel drop it once every process still
  using it lets go, printing the pid and command name of each of those processes.
- `-force` (default: `false`): lazily unmount and abort the kernel-side FUSE
  connection so blocked programs get errors instead of hanging. For a mount wedged by
  a dead or deadlocked daemon. Needs no root for mounts you own.

## status

```
proton-drive-fs status [-config path] [mountpoint]
```

Prints whether the mountpoint is mounted, the running daemon's pid and version, this
binary's version, transfers in flight, and whether syncing is paused; with a version
mismatch it also prints the unmount-then-mount command to fix it. With no argument it
uses the config file's `mountpoint`, then the tray's remembered mountpoint, then falls
back to `~/ProtonDrive`.

## tray

```
proton-drive-fs tray [-config path] [-mountpoint ~/ProtonDrive]
```

Runs a status icon in the system tray. See [Tray](tray.md) for the menu, icon states,
and desktop integration.

## logout

```
proton-drive-fs logout
```

Revokes the session with Proton and removes the session file.

## version

```
proton-drive-fs version
```

Prints the binary's version.

## config

```
proton-drive-fs config init [-config path] [-force]
proton-drive-fs config show [-config path] [flags...]
```

Manages the TOML config file every `login`, `mount`, and `tray` flag also has a key
in. See [Configuration](configuration.md) for the precedence between defaults, the
file, and a flag, the full key table, and what `config init` and `config show` print.

## Systemd user units

Two user units live in `contrib/systemd/`: `proton-drive-fs.service` keeps the mount
running, and `proton-drive-fs-tray.service` keeps the tray icon running with the
graphical session. Copy the ones you want to `~/.config/systemd/user/` (or run
`make install`, which does this for you), then enable them:

```
systemctl --user enable --now proton-drive-fs
systemctl --user enable --now proton-drive-fs-tray
```

Both units run the binary from `~/.local/bin`; edit `ExecStart` if yours lives
elsewhere. The mount unit runs `mount -foreground`, so its output goes to the journal
as part of the unit.

## make restart

```
make restart
```

Unmounts the mountpoint (default `~/ProtonDrive`, override with `MP=<path>`), rebuilds
the binary, and remounts it. Use this after changing code, or after a `git pull`, so
the running daemon always matches the binary on disk.
