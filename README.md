# proton-drive-linux-fs

FUSE virtual filesystem for Proton Drive on Linux.

## What it does

proton-drive-fs mounts Proton Drive as a local folder through FUSE. Files and folders are listed from Proton's metadata; the bytes of a file download only when something opens it. Remote changes reach the mount through Proton's event feed, which invalidates the affected directory listings and cached file content. Writes are buffered to a local temp file and uploaded to Proton in full when the file closes.

## Status

Early and unofficial. Not affiliated with, endorsed by, or supported by Proton AG. The tested surface is small: basic read, write, create, delete, rename and move operations on one Proton Drive share. Expect bugs. Report them on the [issue tracker](https://github.com/khaosdoctor/proton-drive-linux-fs/issues).

## Install

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

proton-drive-fs is one binary with five subcommands: `login`, `mount`, `unmount`, `logout`, `version`.

### Log in

```
proton-drive-fs login
```

Prompts for username, password, and a TOTP code if two-factor is enabled. On first login Proton may require human verification (CAPTCHA, email code, or SMS code); for a CAPTCHA the CLI prints the verify.proton.me URL and opens it unless you pass `-no-browser` (default: false), in which case open the URL yourself. Solve the CAPTCHA there, then press Enter in the terminal. Force a specific method with `-hv-method captcha|email|sms` (default: none forced, tried in the order email, sms, captcha).

Repeated failed login attempts can trigger a temporary lock on the account from Proton's side.

A successful login writes a session file to `$XDG_CONFIG_HOME/proton-drive-fs/session.json` (falls back to `~/.config/proton-drive-fs/session.json`), mode 0600 in a 0700 directory. The file holds the account username, the Proton session UID, and the access and refresh tokens. The salted key password derived from the account password unlocks the drive's encryption keys on later runs without asking for the password again; it goes to the OS keyring (Secret Service, for example GNOME Keyring or KWallet) when one is available, and otherwise stays in the session file itself with mode 0600. `logout` removes both.

### Mount

```
proton-drive-fs mount <mountpoint> [-debug] [-ttl 30s] [-poll 10s] [-cache-dir path] [-cache-size 1GiB] [-foreground]
```

If the mountpoint does not exist, mount says so and creates it. By default mount detaches into the background, waits until the filesystem is mounted, and writes the daemon's log to `$XDG_STATE_HOME/proton-drive-fs/mount.log` (falling back to `~/.local/state/proton-drive-fs/mount.log`).

- `-debug` (default: false): enable FUSE debug logging.
- `-ttl` (default: 30s): how long a directory listing stays cached before it is fetched again.
- `-poll` (default: 10s): how often the event feed is polled for remote changes.
- `-cache-dir` (default: `$XDG_CACHE_HOME/proton-drive-fs/blocks`, falls back to `~/.cache/proton-drive-fs/blocks`): where downloaded, decrypted file blocks are stored on disk so they survive a remount.
- `-cache-size` (default: 1GiB): the total size the on-disk block cache is allowed to use; accepts suffixes like `512MiB` or `2GiB`. A value of 0 or less disables the on-disk cache.
- `-foreground` (default: false): stay attached to the terminal and log to stderr; used by the systemd unit.

### Unmount

```
proton-drive-fs unmount <mountpoint>
```

Runs `fusermount3 -u` (or `fusermount -u` if `fusermount3` is not on `PATH`).

### Log out

```
proton-drive-fs logout
```

Revokes the session with Proton and removes the session file.

### Systemd service

Copy `contrib/systemd/proton-drive-fs.service` to `~/.config/systemd/user/`, then enable it:

```
systemctl --user enable --now proton-drive-fs
```

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
