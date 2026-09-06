# proton-drive-linux-fs

FUSE virtual filesystem for Proton Drive on Linux.

## What it does

proton-drive-fs mounts Proton Drive as a local folder through FUSE. Files and folders are listed from Proton's metadata; the bytes of a file download only when something opens it. Remote changes reach the mount through Proton's event feed, which invalidates the affected directory listings and cached file content. Writes are buffered to a local temp file and uploaded to Proton in full when the file closes.

## Status

Early and unofficial. Not affiliated with, endorsed by, or supported by Proton AG. The tested surface is small: basic read, write, create, delete, rename and move operations on one Proton Drive share. Expect bugs. Report them on the [issue tracker](https://github.com/khaosdoctor/proton-drive-linux-fs/issues).

## Install

From source:

```
git clone https://github.com/khaosdoctor/proton-drive-linux-fs
cd proton-drive-linux-fs
make build
make install
```

The `make build` command places the binary in the repository root. Run `make help` to see all available targets.

From a [GitHub Release](https://github.com/khaosdoctor/proton-drive-linux-fs/releases):

```
tar -xzf proton-drive-fs_linux_amd64.tar.gz
sudo install -m 755 proton-drive-fs /usr/local/bin/proton-drive-fs
```

With Go:

```
go install github.com/khaosdoctor/proton-drive-linux-fs/cmd/proton-drive-fs@latest
```

Container image:

```
docker pull ghcr.io/khaosdoctor/proton-drive-linux-fs:latest
```

See [Docker](#docker) for how to run it.

Requirements: Linux, FUSE 3 (the `fuse3` package on most distributions), and access to `/dev/fuse`.

## Usage

proton-drive-fs is one binary with seven subcommands: `login`, `mount`, `status`, `unmount`, `tray`, `logout`, `version`.

### Log in

```
proton-drive-fs login
```

Prompts for username, password, and a TOTP code if two-factor is enabled. On first login Proton may require human verification (CAPTCHA, email code, or SMS code); for a CAPTCHA the CLI prints the verify.proton.me URL and opens it unless you pass `-no-browser` (default: false), in which case open the URL yourself. Solve the CAPTCHA there, then press Enter in the terminal. Force a specific method with `-hv-method captcha|email|sms` (default: none forced, tried in the order email, sms, captcha).

Repeated failed login attempts can trigger a temporary lock on the account from Proton's side.

A successful login writes a session file to `$XDG_CONFIG_HOME/proton-drive-fs/session.json` (falls back to `~/.config/proton-drive-fs/session.json`), mode 0600 in a 0700 directory. The file holds the account username, the Proton session UID, and the access and refresh tokens. The salted key password derived from the account password unlocks the drive's encryption keys on later runs without asking for the password again; it goes to the OS keyring (Secret Service, for example GNOME Keyring or KWallet) when one is available, and otherwise stays in the session file itself with mode 0600. `logout` removes both.

### Mount

```
proton-drive-fs mount <mountpoint> [-debug] [-ttl 30s] [-poll 10s] [-op-timeout 60s] [-cache-dir path] [-cache-size 1GiB] [-large-file 300MiB] [-thumbnails] [-thumbnail-dir path] [-deny-readers names] [-foreground]
```

If the mountpoint does not exist, mount says so and creates it. By default mount detaches into the background and waits until the filesystem is mounted. When `systemd-cat` is on `PATH` the daemon's output goes to the journal under the identifier `proton-drive-fs`, readable with `journalctl --user -t proton-drive-fs`; without it, the output is appended to `$XDG_STATE_HOME/proton-drive-fs/mount.log` (falling back to `~/.local/state/proton-drive-fs/mount.log`).

- `-debug` (default: false): enable FUSE debug logging.
- `-ttl` (default: 30s): how long a directory listing stays cached before it is fetched again.
- `-poll` (default: 10s): how often the event feed is polled for remote changes.
- `-op-timeout` (default: 60s): deadline for one filesystem operation's network calls (listing, open, read, upload, mkdir, remove, rename); an operation stuck past this returns an error instead of hanging the caller. Uploads scale past this for large files.
- `-cache-dir` (default: `$XDG_CACHE_HOME/proton-drive-fs/blocks`, falls back to `~/.cache/proton-drive-fs/blocks`): where downloaded, decrypted file blocks are stored on disk so they survive a remount.
- `-cache-size` (default: 1GiB): the total size the on-disk block cache is allowed to use; accepts suffixes like `512MiB` or `2GiB`. A value of 0 or less disables the on-disk cache.
- `-large-file` (default: 300MiB): files larger than this are still read lazily block by block but their blocks are not stored in the on-disk cache, so one large file cannot evict everything else; 0 disables the threshold.
- `-thumbnails` (default: true): write the preview image Proton stores for a file into the freedesktop thumbnail cache when a folder is listed.
- `-thumbnail-dir` (default: `$XDG_CACHE_HOME/thumbnails`, falls back to `~/.cache/thumbnails`): the thumbnail cache directory to write into. This is the shared directory file managers read, not a directory of its own.
- `-deny-readers` (default: `tracker-miner-fs,tracker-extract,localsearch,baloo_file,baloo_file_extractor,tumblerd,ffmpegthumbnailer,totem-video-thumbnailer,gdk-pixbuf-thumbnailer,gnome-desktop-thumbnailer,evince-thumbnailer`): comma-separated process names refused a read of a file above `-large-file`. Passing a value replaces the default list; `-deny-readers ""` turns the refusal off.
- `-foreground` (default: false): stay attached to the terminal and log to stderr; used by the systemd unit.

`mount` refuses to attach to a mountpoint that is already mounted, printing the running daemon's pid and version when the status file has them, so a rebuild whose earlier unmount failed as busy never gets mistaken for actually running the new binary; `make restart` (optionally `MP=<mountpoint>`, default `~/ProtonDrive`) unmounts, rebuilds, and remounts in one step.

### Previews

Proton stores a small thumbnail next to each file it has one for. When a folder is listed, the mount downloads those thumbnails and writes them into the freedesktop thumbnail cache (the directory `-thumbnail-dir` points at), so file managers show previews without opening the files themselves. A thumbnail is a few kilobytes regardless of how large the file is, and it is fetched in the background, so listing a folder is not held up by it.

Some desktops also run thumbnailers and search indexers that open every file they find. On a network filesystem that means downloading everything in a folder just to look at it. Files above `-large-file` are refused to the processes in `-deny-readers`, which are the dedicated thumbnailer and indexer binaries; those opens fail with a permission error and nothing is downloaded. Applications the user launches to open a file are not on the list and are unaffected.

### Unmount

```
proton-drive-fs unmount [-force] [-wait 5s] <mountpoint>
```

Runs `fusermount3 -u` (or `fusermount -u` if `fusermount3` is not on `PATH`); if the mountpoint is busy, it retries every 500ms for up to `-wait` (default 5s). If it is still busy after that, it falls back to a lazy unmount, which detaches the mount right away and lets the kernel drop it once every process still using it lets go, and prints the pid and command name of each of those processes. When the daemon has died or deadlocked and programs are stuck on the mount, `-force` lazily unmounts and aborts the kernel-side FUSE connection so blocked programs get errors instead of hanging; it needs no root for mounts you own.

### Status

```
proton-drive-fs status [mountpoint]
```

Prints whether the mountpoint is mounted, the running daemon's pid and version, this binary's version, transfers in flight, and whether syncing is paused; with a version mismatch it also prints the unmount-then-mount command to fix it. With no argument it uses the tray's remembered mountpoint, falling back to `~/ProtonDrive`.

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

Both units run the binary from `~/.local/bin`; edit `ExecStart` if yours lives elsewhere. The mount unit runs `mount -foreground`, so its output goes to the journal as part of the unit. A mount started outside systemd logs to the journal too when `systemd-cat` is installed: `journalctl --user -t proton-drive-fs`.

## Tray

```
proton-drive-fs tray [-mountpoint ~/ProtonDrive]
```

Runs a status icon in the system tray over StatusNotifierItem, which is what Waybar, KDE Plasma and the GNOME AppIndicator extension speak. With no `-mountpoint` the tray reuses the last mountpoint it was given and falls back to `~/ProtonDrive`; the choice is stored in `$XDG_CONFIG_HOME/proton-drive-fs/tray.json` so the menu keeps working after a restart.

The icon is a cloud in one of four states, picked in this order:

- Hollow outline: no saved session, or nothing mounted. The status line in the menu says which of the two it is.
- Two bars in the corner: polling is paused.
- A dot in the corner: a download or an upload is in flight.
- Solid: mounted, logged in, nothing moving.

The menu holds a status line (`Mounted at <path>`, `Not mounted` or `Not logged in`), then items shown only when they apply: `Mount` when logged in but not mounted, `Unmount` and `Restart mount` when mounted, `Pause syncing` or `Resume syncing` when mounted, `Open folder` when mounted, `Open logs`, `Log in` when logged out, `Log out` when logged in, and `Quit`. `Mount` and `Unmount` run this same binary, so a mount started from the menu is the same detached mount you get from a shell and it survives the tray closing. `Quit` only closes the icon; it never unmounts.

When the status line gets a ` (daemon X, restart needed)` suffix, the running daemon is an older build than the tray itself, usually because a rebuild's earlier unmount failed as busy; click `Restart mount` to unmount and remount with the current binary.

`Log in` needs a terminal because it prompts for the password. The tray starts the first terminal it finds on `PATH`, trying `$TERMINAL` first and then `x-terminal-emulator`, `kitty`, `alacritty`, `foot`, `gnome-terminal`, `konsole`, `xterm`. When none of them is installed, the status line shows the command to run yourself for ten seconds. `Open logs` follows the journal in a terminal when the mount logs there, and otherwise opens the log file with `xdg-open`.

Pause stops one thing: the poll of Proton's event feed. Remote changes stop reaching the mount until you resume, while reads and writes keep working throughout. It is a marker file at `$XDG_RUNTIME_DIR/proton-drive-fs/paused` (falling back to `$XDG_STATE_HOME/proton-drive-fs/paused`) that the mount checks on every poll tick, so it also applies to a mount the tray did not start.

For the syncing state the mount writes `$XDG_RUNTIME_DIR/proton-drive-fs/status.json` (same fallback) once a second with its pid, version, and the number of transfers in flight. A snapshot older than ten seconds counts as no mount running; whether the filesystem is mounted always comes from `/proc/self/mounts` instead.

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

The auth layer logs in against Proton's API, handling two-factor codes and human verification challenges. It derives the account's key password from the login password and Proton's stored salts, then persists everything needed to restore the session, including that key password, to the local session file.

The drive layer wraps Proton's Drive API into a tree of nodes (files and folders), exposing operations to list children, open a file for streamed reading, upload a new revision, create a folder, move, and trash. It also polls Proton's event feed and turns raw events into a normalized stream the FUSE layer reacts to.

The FUSE layer publishes that tree as a mounted filesystem with go-fuse. Directory listings are cached per folder for the configured TTL and refetched on expiry or on a matching remote event. Opening a file for reading streams and caches its blocks on demand instead of downloading the whole file up front. Opening a file for writing buffers the new content to a local temp file and uploads it as a new revision when the file closes.

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
