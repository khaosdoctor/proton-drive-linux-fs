# proton-drive-linux-fs

Docs: https://oss.lsantos.dev/proton-drive-linux-fs/

FUSE virtual filesystem for Proton Drive on Linux.

## What it does

proton-drive-fs mounts Proton Drive as a local folder through FUSE. Files and folders are listed from Proton's metadata; the bytes of a file download only when something opens it. Remote changes reach the mount through Proton's event feed, which invalidates the affected directory listings and cached file content. Writes are buffered to a local temp file and uploaded to Proton in full when the file closes.

## Status

Early and unofficial. Not affiliated with, endorsed by, or supported by Proton AG. Used live, on one Proton Drive share: login, mount, read, create, and rename. Write, delete, move, the tray, and thumbnail previews have not seen the same real-world use yet. Expect bugs. Report them on the [issue tracker](https://github.com/khaosdoctor/proton-drive-linux-fs/issues).

## Quick start

Full copy-paste flows are in the [quick start guide](https://oss.lsantos.dev/proton-drive-linux-fs/quickstart/):

- Installed package, `go install`, or `make install`: `proton-drive-fs login`, then `proton-drive-fs mount ~/ProtonDrive`.
- systemd user units: `systemctl --user enable --now proton-drive-fs` (and `-tray` for the icon).
- Docker: `docker run ... login`, then `docker run ... mount -foreground /mnt/protondrive`, both with the same bind mounts.

## Install

Four ways to get the binary. Full requirements and the container run command are in
the [install docs](https://oss.lsantos.dev/proton-drive-linux-fs/install/).

From a [GitHub Release](https://github.com/khaosdoctor/proton-drive-linux-fs/releases):

```
tar -xzf proton-drive-fs_linux_amd64.tar.gz
sudo install -m 755 proton-drive-fs /usr/local/bin/proton-drive-fs
```

With Go:

```
go install github.com/khaosdoctor/proton-drive-linux-fs/cmd/proton-drive-fs@latest
```

From source:

```
git clone https://github.com/khaosdoctor/proton-drive-linux-fs
cd proton-drive-linux-fs
make build
make install
```

`make install` also sets up the desktop entry and the systemd user units. Run
`make help` to see every target.

Container image:

```
docker pull ghcr.io/khaosdoctor/proton-drive-linux-fs:latest
```

See [Docker](#docker) for how to run it.

Packages: `.deb`, `.rpm`, `.apk`, and Arch packages are attached to each [GitHub release](https://github.com/khaosdoctor/proton-drive-linux-fs/releases). Install with the matching package manager, then enable the tray with `systemctl --user enable --now proton-drive-fs-tray`. Exact commands per format are in the [install docs](https://oss.lsantos.dev/proton-drive-linux-fs/install/).

### Arch Linux (AUR)

```
yay -S proton-drive-fs-bin
```

That installs a prebuilt binary. To build from source instead:

```
yay -S proton-drive-fs
```

Both pull in `fuse3` as a dependency.

Requirements: Linux, FUSE 3 (the `fuse3` package on most distributions), and access to `/dev/fuse`.

## Usage

proton-drive-fs is one binary with eight subcommands: `login`, `mount`, `status`, `unmount`, `tray`, `logout`, `version`, `config`.

Every flag below also has a matching key in the [config file](#configuration); a flag passed on the command line always wins.

### Log in

```
proton-drive-fs login [-config path]
```

Prompts for username, password, and a TOTP code if two-factor is enabled. On first login Proton may require human verification (CAPTCHA, email code, or SMS code); for a CAPTCHA the CLI prints the verify.proton.me URL and opens it unless you pass `-no-browser` (default: false), in which case open the URL yourself. Solve the CAPTCHA there, then press Enter in the terminal. Force a specific method with `-hv-method captcha|email|sms` (default: none forced, tried in the order email, sms, captcha).

Repeated failed login attempts can trigger a temporary lock on the account from Proton's side.

A successful login writes a session file to `$XDG_CONFIG_HOME/proton-drive-fs/session.json` (falls back to `~/.config/proton-drive-fs/session.json`), mode 0600 in a 0700 directory. The file holds the account username, the Proton session UID, and the access and refresh tokens. The salted key password derived from the account password unlocks the drive's encryption keys on later runs without asking for the password again; it goes to the OS keyring (Secret Service, for example GNOME Keyring or KWallet) when one is available, and otherwise stays in the session file itself with mode 0600. `logout` removes both.

### Mount

```
proton-drive-fs mount [<mountpoint>] [-config path] [-debug] [-ttl 30s] [-poll 10s] [-op-timeout 60s] [-cache-dir path] [-cache-size 2GiB] [-large-file 300MiB] [-thumbnails] [-thumbnail-dir path] [-deny-readers names] [-max-uploads 5] [-max-downloads 8] [-foreground] [-log-level info] [-log-stderr]
```

`<mountpoint>` is required unless the config file sets `mountpoint`. If it does not exist, mount says so and creates it. By default mount detaches into the background and waits until the filesystem is mounted. The daemon logs to the systemd journal itself under the identifier `proton-drive-fs`, readable with `journalctl --user -t proton-drive-fs`; see [Logs](#logs) for levels and the file fallback when there's no journal.

- `-debug` (default: false): enable FUSE debug logging.
- `-ttl` (default: 30s): how long a directory listing stays cached before it is fetched again.
- `-poll` (default: 10s): how often the event feed is polled for remote changes.
- `-op-timeout` (default: 60s): deadline for one filesystem operation's network calls (listing, open, read, upload, mkdir, remove, rename); an operation stuck past this returns an error instead of hanging the caller. Uploads scale past this for large files.
- `-cache-dir` (default: `$XDG_CACHE_HOME/proton-drive-fs`, falls back to `~/.cache/proton-drive-fs`): where downloaded, decrypted file blocks (under `blocks/`) and persisted directory listings (under `listings/`) are stored on disk so they survive a remount.
- `-cache-size` (default: 2GiB): the total size the on-disk cache is allowed to use, shared by blocks and persisted listings together; accepts suffixes like `512MiB` or `2GiB`. A value of 0 or less disables the on-disk cache, both kinds.
- `-large-file` (default: 300MiB): files larger than this are still read lazily block by block but their blocks are not stored in the on-disk cache, so one large file cannot evict everything else; 0 disables the threshold.
- `-thumbnails` (default: true): write the preview image Proton stores for a file into the freedesktop thumbnail cache when a folder is listed.
- `-thumbnail-dir` (default: `$XDG_CACHE_HOME/thumbnails`, falls back to `~/.cache/thumbnails`): the thumbnail cache directory to write into. This is the shared directory file managers read, not a directory of its own.
- `-deny-readers` (default: `tracker-miner-fs,tracker-extract,localsearch,baloo_file,baloo_file_extractor,tumblerd,ffmpegthumbnailer,totem-video-thumbnailer,gdk-pixbuf-thumbnailer,gnome-desktop-thumbnailer,evince-thumbnailer`): comma-separated process names refused a read of a file above `-large-file`. Passing a value replaces the default list; `-deny-readers ""` turns the refusal off.
- `-max-uploads` (default: 5): how many files upload at once. Copying a folder in hands the mount every file at once; the rest wait in line instead of opening a connection each. 0 or less removes the cap.
- `-max-downloads` (default: 8): how many file blocks download at once, across every open file. 0 or less removes the cap.
- `-foreground` (default: false): stay attached to the terminal instead of detaching into the background; used by the systemd unit.
- `-log-level` (default: info): log verbosity, one of `debug`, `info`, `warn`, `error`. See [Logs](#logs).
- `-log-stderr` (default: false): force logging to stderr instead of the systemd journal, useful with `-foreground` when working at a terminal.

`mount` refuses to attach to a mountpoint that is already mounted, printing the running daemon's pid and version when the status file has them, so a rebuild whose earlier unmount failed as busy never gets mistaken for actually running the new binary; `make restart` (optionally `MP=<mountpoint>`, default `~/ProtonDrive`) unmounts, rebuilds, and remounts in one step.

A cold directory (nothing cached in memory yet, for example right after mount) is served from the persisted listing cache when one exists, so a folder you've listed before shows up instantly instead of waiting on the network; a background refresh follows the same TTL and event-driven invalidation as everything else, and refreshes the persisted copy once it lands.

### Previews

Proton stores a small thumbnail next to each file it has one for; the mount writes it into the freedesktop thumbnail cache when a folder is listed, so file managers show previews without opening the file itself. Thumbnailers and search indexers that would otherwise open every file in a folder are refused a read of anything above `-large-file`. See [How it works](https://oss.lsantos.dev/proton-drive-linux-fs/how-it-works/#previews) for the full explanation.

### Unmount

```
proton-drive-fs unmount [-force] [-wait 5s] <mountpoint>
```

Runs `fusermount3 -u` (or `fusermount -u` if `fusermount3` is not on `PATH`); if the mountpoint is busy, it retries every 500ms for up to `-wait` (default 5s). If it is still busy after that, it falls back to a lazy unmount, which detaches the mount right away and lets the kernel drop it once every process still using it lets go, and prints the pid and command name of each of those processes. When the daemon has died or deadlocked and programs are stuck on the mount, `-force` lazily unmounts and aborts the kernel-side FUSE connection so blocked programs get errors instead of hanging; it needs no root for mounts you own.

### Status

```
proton-drive-fs status [-config path] [mountpoint]
```

Prints whether the mountpoint is mounted, the running daemon's pid and version, this binary's version, transfers in flight, and whether syncing is paused; with a version mismatch it also prints the unmount-then-mount command to fix it. With no argument it uses the config file's `mountpoint`, then the tray's remembered mountpoint, then falls back to `~/ProtonDrive`.

### Log out

```
proton-drive-fs logout
```

Revokes the session with Proton and removes the session file.

### Systemd service

There are two user units in `contrib/systemd/`: `proton-drive-fs.service` keeps the mount running, and `proton-drive-fs-tray.service` keeps the tray icon running with the graphical session. Copy the ones you want to `~/.config/systemd/user/`, then enable them:

```
systemctl --user enable --now proton-drive-fs
systemctl --user enable --now proton-drive-fs-tray
```

Both units run the binary from `~/.local/bin`; edit `ExecStart` if yours lives elsewhere. The mount unit runs `mount -foreground`; either way the daemon logs straight to the journal under the identifier `proton-drive-fs`, see [Logs](#logs).

## Configuration

Every flag `login`, `mount` and `tray` accept also has a key in a TOML config file at `$XDG_CONFIG_HOME/proton-drive-fs/config.toml` (falls back to `~/.config/proton-drive-fs/config.toml`), or wherever `-config <path>` points instead. `status` also reads it for `mountpoint`, when no mountpoint is given on the command line. Values are resolved in this order, each one overriding the last: **built-in defaults** < **config file** < **flag explicitly passed on the command line**.

```
proton-drive-fs config init [-config path] [-force]
proton-drive-fs config show [-config path] [flags...]
```

`config init` writes a fully commented config file with every key at its default value and a one-line explanation; uncomment a line to set it. It refuses to overwrite an existing file unless `-force` is passed. `config show` prints the effective configuration after merging defaults, the file, and any flag passed to `config show` itself, with a trailing comment naming where each value came from (`default`, `file`, or `flag`), useful to check what `mount` or `login` would actually resolve to before running them.

| Key | Flag | Default | Description |
| --- | --- | --- | --- |
| `mountpoint` | (positional for `mount`, `-mountpoint` for `tray`) | (none) | Default mountpoint `mount` and `tray` use when none is given on the command line. |
| `ttl` | `-ttl` | `30s` | How long a directory listing stays cached before it is fetched again. |
| `poll` | `-poll` | `10s` | How often the event feed is polled for remote changes. |
| `op_timeout` | `-op-timeout` | `60s` | Deadline for one filesystem operation's network calls. |
| `cache_dir` | `-cache-dir` | `$XDG_CACHE_HOME/proton-drive-fs` | Where downloaded file blocks and persisted directory listings are stored on disk. |
| `cache_size` | `-cache-size` | `2GiB` | Total size the on-disk cache (blocks and listings together) may use; `"0"` disables both. |
| `large_file` | `-large-file` | `300MiB` | Files larger than this bypass the on-disk block cache; `"0"` disables the threshold. |
| `thumbnails` | `-thumbnails` | `true` | Write Proton's stored previews into the freedesktop thumbnail cache. |
| `thumbnail_dir` | `-thumbnail-dir` | `$XDG_CACHE_HOME/thumbnails` | Freedesktop thumbnail cache directory. |
| `deny_readers` | `-deny-readers` | see [Mount](#mount) | Process names refused a read of a file above `large_file`; empty allows all. |
| `max_uploads` | `-max-uploads` | `5` | How many files upload at once. |
| `max_downloads` | `-max-downloads` | `8` | How many file blocks download at once. |
| `log_level` | `-log-level` | `info` | Log verbosity: `debug`, `info`, `warn` or `error`. |
| `log_stderr` | `-log-stderr` | `false` | Force logging to stderr instead of the systemd journal. |
| `foreground` | `-foreground` | `false` | Stay attached to the terminal instead of detaching into the background. |
| `hv_method` | `-hv-method` | (none) | Force a human verification method at login: `captcha`, `email` or `sms`. |
| `no_browser` | `-no-browser` | `false` | Do not open a browser for human verification at login. |

The persisted listing cache stores decrypted file and folder names on disk (under `cache_dir/listings/`, mode 0600) so a folder listed once loads instantly on the next cold start, same trade-off the session file already makes for the account password. It shares `cache_size`'s byte budget with the block cache (under `cache_dir/blocks/`); set `cache_size = "0"` to disable both. See [Troubleshooting](https://oss.lsantos.dev/proton-drive-linux-fs/troubleshooting/#logs) for where the cache files live and log levels for cache hits and misses.

## Tray

```
proton-drive-fs tray [-config path] [-mountpoint ~/ProtonDrive]
```

Runs a status icon in the system tray over StatusNotifierItem, which is what Waybar, KDE Plasma and the GNOME AppIndicator extension speak. The icon shows one of four states (no session or nothing mounted, paused, a transfer in flight, or idle and mounted), and the menu offers mount, unmount, pause, open folder, open logs, open debug logs, log in, log out, and quit. See [Tray](https://oss.lsantos.dev/proton-drive-linux-fs/tray/) for the full menu, icon states, and pause semantics.

While a transfer is in progress the tray's tooltip shows it: `Uploading Files/report.pdf 40%` for the first one, with `and N more` appended when several are running at once. With nothing moving, the tooltip falls back to the same status line the menu shows. The tray polls once a second while a transfer is active, and once every two seconds otherwise.

The menu lists up to three of the transfers in progress as disabled lines, in the same format as the tooltip: `Uploading <path> 40%` or `Downloading <path>`. Below them, a `Recent` submenu lists the last ten finished transfers, oldest at the bottom: `✓ <path> (<size>)` for one that finished, `✗ <path>: <error>` for one that failed. Clicking a `Recent` entry opens its containing folder. Both sections are hidden when there is nothing to show.

`About proton-drive-fs` opens a dialog with the project name, version and commit, links to the repository, the docs and the issue tracker, and every third-party dependency's license. It uses `zenity --text-info` when zenity is installed, and falls back to writing an HTML file and opening it with `xdg-open` otherwise. The same text is available without a GUI: `proton-drive-fs about` prints it to stdout.

The license list is generated, not hand-maintained: `go generate ./internal/about` walks the module's dependencies, copies each one's `LICENSE` file out of the local module cache into `internal/about/licenses/`, and writes an index the About dialog reads at runtime. Run it again after adding or upgrading a dependency and commit the result.

### Desktop entry

```
install -Dm644 contrib/proton-drive-fs.desktop ~/.local/share/applications/proton-drive-fs.desktop
install -Dm644 contrib/icons/proton-drive-fs.png ~/.local/share/icons/hicolor/64x64/apps/proton-drive-fs.png
```

To start the tray with the session instead, use the `proton-drive-fs-tray.service` user unit described under [Systemd service](#systemd-service).

### Tray hosts

- Waybar: add the `tray` module to `modules-right` and `"tray": {}` to the config.
- KDE Plasma: works with no setup.
- GNOME: needs the AppIndicator and KStatusNotifierItem Support extension; GNOME Shell has no tray of its own.

## How it works

Three layers: auth logs in against Proton's API and derives the key password that unlocks the drive's encryption keys; drive wraps Proton's Drive API into a tree of files and folders and polls the event feed for remote changes; FUSE publishes that tree as a mount with go-fuse, reading files lazily in blocks and buffering writes locally until the file closes. See [How it works](https://oss.lsantos.dev/proton-drive-linux-fs/how-it-works/) for the full description of each layer.

## Logs

The daemon logs structured entries to the systemd user journal under `proton-drive-fs`, asynchronously so a slow or backed-up log write never stalls a filesystem operation. `-log-stderr`, or no journal to write to, falls back to a plain log file instead. See [Troubleshooting](https://oss.lsantos.dev/proton-drive-linux-fs/troubleshooting/#logs) for levels, reading commands, and the file fallback.

## Troubleshooting

CAPTCHA and locked-account prompts during login, a mountpoint stuck "busy" on unmount, and a stale daemon left running after a rebuild are the most common issues; see [Troubleshooting](https://oss.lsantos.dev/proton-drive-linux-fs/troubleshooting/) for the fix for each, plus where the session, cache, and log files live.

## Docker

```
docker run --rm -it \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor:unconfined \
  -v ~/.config/proton-drive-fs:/root/.config/proton-drive-fs \
  -v ~/ProtonDrive:/mnt/protondrive:rshared \
  ghcr.io/khaosdoctor/proton-drive-linux-fs:latest \
  mount /mnt/protondrive
```

Run `login` the same way first, with the same config bind mount, to create the session. `--device /dev/fuse`, `--cap-add SYS_ADMIN`, and `--security-opt apparmor:unconfined` are what FUSE needs to create a mount inside a container. The bind mount on the config directory keeps the session across container runs. The bind mount on the mountpoint needs `:rshared` propagation for the mount created inside the container to become visible on the host; without it the mount stays confined to the container's own mount namespace.

## Limitations

- Writes buffer the whole file locally and upload it in full on close; there is no partial write to the remote file.
- Only one writer per file at a time.
- The on-disk block cache is bounded by size only (least recently used eviction); there is no integrity re-check of cached blocks.
- No trash/restore support and no shared-drive support.
- The API this tool talks to is unofficial and can change or break without notice.
- Only the primary Proton share is mounted; other shares are not exposed.

## License

MIT. See [LICENSE](LICENSE).
