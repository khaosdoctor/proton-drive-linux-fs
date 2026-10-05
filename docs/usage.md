# Usage

proton-drive-fs is a single binary with a few subcommands: `login`, `mount`, `unmount`, `status`, `tray`, `logout`, `autostart`, `version`, `about`, and `config`.

Every flag you can pass to `login`, `mount`, and `tray` also has a matching key in the config file, so you don't need to type them every time. Check [Configuration](configuration.md) to see where the file lives, which keys it has, and how flags and the file get resolved together.

## Operation models

You can keep proton-drive-fs running in three ways, it depends on how you use it.

**Manual.** You run `mount` when you want the drive and `unmount` when you're done. The daemon detaches into the background when you mount, but nothing supervises it, so it only stays up until you unmount, log out, or it crashes. You can use `status` to check if it's still mounted. This is good if you're trying the tool out or if you only use it once in a while.

**systemd (recommended for daily use).** The `proton-drive-fs` user unit runs `mount -foreground` without a mountpoint, so you need to set `mountpoint` in `config.toml` first. systemd starts it with your graphical session (or at boot if you're on a headless machine), restarts it if it fails, and runs `unmount` when it stops. All the output goes to the journal. You can set it up with [autostart](#autostart), or follow the [Quick start](quickstart.md#systemd-user-units) walkthrough.

**Docker.** The container image only runs the CLI, so there's no tray and no desktop integration. Check [Install](install.md#container-image) for the details.

## login

```
proton-drive-fs login [-config path] [-no-browser] [-hv-method captcha|email|sms]
```

This asks for your username, password, and a TOTP code if you have two-factor enabled.

- `-no-browser` (default: `false`): don't open a browser for human verification. On the first login Proton might ask you to verify you're human (CAPTCHA, email code, or SMS code). For a CAPTCHA the CLI prints the verify.proton.me URL and opens it for you, unless you pass this flag, then you need to open the URL yourself.
- `-hv-method` (default: none): forces a specific verification method (`captcha`, `email`, or `sms`). When you don't pass it, we try whatever Proton offers in the order email, sms, captcha.

When the login works, we write a session file to `$XDG_CONFIG_HOME/proton-drive-fs/session.json` (or `~/.config/proton-drive-fs/session.json` if that's not set), with mode 0600 inside a 0700 directory. We also derive a salted key password from your account password, which is what unlocks the drive's encryption keys on later runs. If you have an OS keyring, the key password goes there, otherwise it stays in the session file with mode 0600. Running `logout` removes both.

## mount

```
proton-drive-fs mount [<mountpoint>] [-config path] [flags]  (required unless mountpoint is set in the config file)
```

There's no default mountpoint, so you need to pass `<mountpoint>` unless you set `mountpoint` in the config file. If the directory doesn't exist, mount creates it for you. By default mount detaches into the background and waits until the filesystem is mounted before returning. Logs go to the systemd journal under the `proton-drive-fs` identifier. Check [Read the logs](troubleshooting.md#read-the-logs) for the log levels and the file fallback when there's no journal, and read [Indexer spam](troubleshooting.md#indexer-spam) before you pick a mountpoint.

| Flag | Default | Meaning |
|---|---|---|
| `-debug` | `false` | Enables FUSE debug logging. |
| `-ttl` | `30s` | How long a directory listing stays cached before we fetch it again. |
| `-poll` | `10s` | How often we poll the event feed for remote changes. |
| `-op-timeout` | `60s` | Deadline for the network calls of a single filesystem operation (listing, open, read, upload, mkdir, remove, rename). If an operation takes longer than this, it returns an error instead of hanging whoever called it. Uploads get more time for large files. |
| `-cache-dir` | `$XDG_CACHE_HOME/proton-drive-fs` (or `~/.cache/proton-drive-fs`) | Where we store the downloaded and decrypted file blocks (under `blocks/`) and the persisted directory listings (under `listings/`), so they survive a remount. |
| `-cache-size` | `2GiB` | How much space the on-disk cache can use in total, blocks and listings together. You can use suffixes like `512MiB` or `2GiB`. Setting it to 0 or less disables the on-disk cache for both. |
| `-large-file` | `300MiB` | Files bigger than this are still read lazily block by block, but we don't keep their blocks in the on-disk cache, so one huge file can't push everything else out. 0 disables the threshold. |
| `-thumbnails` | `true` | Writes preview images into the freedesktop thumbnail cache when you list a folder. We use Proton's stored previews when they exist, and for file types Proton has no preview for, we use the system thumbnailers (found through their `.thumbnailer` files). |
| `-thumbnail-dir` | `$XDG_CACHE_HOME/thumbnails` (or `~/.cache/thumbnails`) | The thumbnail cache directory we write into. This is the same shared directory your file manager reads from. |
| `-deny-readers` | check below | Comma-separated list of process names that can't read files bigger than `-large-file`. Passing a value replaces the default list, and `-deny-readers ""` turns this off. Thumbnailers found through `.thumbnailer` files are always blocked, no matter the list or the file size. |
| `-exclude` | check below | Comma-separated filename patterns we hide from listings and never upload. Apps can still create files under these names (for example a slicer saving to `model.3mf.tmp` and renaming it): the file stays local, and gets uploaded only if it is renamed to a name outside the list. Plain patterns use Go's `filepath.Match` (shell glob), and if you prefix a pattern with `re:` it's a regexp. Passing a value replaces the default list, and `-exclude ""` turns exclusion off. |
| `-upload-delay` | `5s` | How long a file waits after it closes before we upload it. Many apps save by writing a temp file (FreeCAD writes `model.FCStd.<uuid>`), renaming the old file to a backup, then renaming the temp file over the original. Waiting lets that whole sequence finish locally, so only the final file goes to Proton. A rename starts the wait over under the new name, opening the file for writing again takes it back, and deleting it cancels the upload. The file shows in listings and reads normally while it waits, and unmounting uploads whatever is still waiting. `0s` uploads as soon as the file closes. |
| `-upload-delays` | (none) | Comma-separated per-pattern overrides of `-upload-delay`, as `pattern=duration`, for example `*.bak=1m,*.log=0s`. The first matching pattern is used, and patterns use the same syntax as `-exclude`. To never upload a kind of file at all, put its pattern in `-exclude`. |
| `-max-uploads` | `5` | How many files we upload at the same time. The rest wait in line instead of each opening its own connection. 0 or less removes the limit. |
| `-max-downloads` | `20` | How many file blocks we download at the same time, counting every open file. 0 or less removes the limit. |
| `-foreground` | `false` | Stays attached to the terminal instead of going to the background. The systemd unit uses this. |
| `-log-level` | `info` | How much to log: `debug`, `info`, `warn`, or `error`. Check [Read the logs](troubleshooting.md#read-the-logs). |
| `-log-stderr` | `false` | Logs to stderr instead of the systemd journal. This is useful with `-foreground` when you're running it in a terminal. |

This is the default `-deny-readers` list:

```
tracker-miner-fs, tracker-extract, localsearch, baloo_file, baloo_file_extractor,
tumblerd, ffmpegthumbnailer, totem-video-thumbnailer, gdk-pixbuf-thumbnailer,
gnome-desktop-thumbnailer, evince-thumbnailer
```

And this is the default `-exclude` list:

```
.DS_Store, Thumbs.db, desktop.ini, ._*, .Spotlight-V100, .Trashes, .fseventsd,
*.swp, *.swo, *.tmp, *~, re:^\.~lock\.
```

If the mountpoint is already mounted, `mount` won't attach to it again, and it prints the pid and version of the daemon that's already running there. Check [Stale daemon after a rebuild](troubleshooting.md#stale-daemon-after-a-rebuild) to see how to deal with that.

When you open a directory we don't have in memory yet (like right after you mount), we serve it from the persisted listing cache if we have one, so a folder you listed before shows up right away instead of waiting on the network. Then we refresh it in the background, following the same TTL and event rules as everything else. Check [Cache layout](how-it-works.md#cache-layout) to see how blocks and listings share the cache directory.

## unmount

```
proton-drive-fs unmount [<mountpoint>] [-config path] [-force] [-wait 5s]  (required unless mountpoint is set in the config file)
```

This runs `fusermount3 -u` (or `fusermount -u` if you don't have `fusermount3` on your `PATH`). You need to pass `<mountpoint>` unless you set `mountpoint` in the config file, which is how the systemd unit's `ExecStop` can run `unmount` without any arguments.

- `-wait` (default: `5s`): if the mountpoint is busy, we retry every 500ms for up to this long. If it's still busy after that, we do a lazy unmount, which detaches the mount right away and lets the kernel drop it once every process lets go of it.
- `-force` (default: `false`): does a lazy unmount and also aborts the FUSE connection in the kernel, so programs that are stuck get errors instead of hanging forever. Use this when the mount is wedged because the daemon died or deadlocked.

Check ["Device or resource busy" on unmount](troubleshooting.md#device-or-resource-busy-on-unmount) for the whole escalation path.

## status

```
proton-drive-fs status [<mountpoint>] [-config path]  (required unless mountpoint is set in the config file)
```

This shows you if the mountpoint is mounted, the pid and version of the running daemon, the version of the binary you ran, the transfers in progress, and if syncing is paused. If the versions don't match, it also prints the unmount and mount commands you need to fix it. When you don't pass a mountpoint, we use the one from the config file.

## tray

```
proton-drive-fs tray [-config path] [-mountpoint <path>]
```

This runs a status icon in your system tray. If you don't pass `-mountpoint`, we use the one in the config file, and if there's none there either, we use the last one the tray used. When we can't find any mountpoint, the tray still starts, but you can't manage the mount from it until you set one. Check [Tray](tray.md) for the menu, the icon states, and the desktop integration.

## logout

```
proton-drive-fs logout
```

This revokes your session with Proton and deletes the session file.

## version

```
proton-drive-fs version
```

Prints the version of the binary.

## config

```
proton-drive-fs config init [-config path] [-force]
proton-drive-fs config show [-config path] [flags...]
```

Manages the TOML config file. The first command you run that reads the config already creates the file for you, so you only need `config init` if you want to rewrite a file that already exists (`-force`) or write one somewhere else (`-config`). Check [Configuration](configuration.md) for all the keys and how flags and the file take precedence over each other.

## autostart

```
proton-drive-fs autostart [-headless | -remove]
```

This sets up the `proton-drive-fs` systemd user unit, enables it, and starts it (or restarts it if it's already running). The unit runs `mount -foreground`, which takes care of the FUSE mount and the tray icon, and the tray only shows up if you have a display server. The unit doesn't pass a mountpoint, so you need to set `mountpoint` in `config.toml` and run `login` before you use this. You can follow the logs with `journalctl --user -u proton-drive-fs -f`.

By default the unit starts together with your graphical session, because that's when your OS keyring can be unlocked. If you installed proton-drive-fs from a package, the unit is already in `/usr/lib/systemd/user/` and `autostart` just enables it. For any other install, we write the unit to `~/.config/systemd/user/`, pointing at the binary you ran `autostart` from.

If you're on a machine without a desktop, pass `-headless`. This writes a unit to `~/.config/systemd/user/` that starts with `default.target` instead, and runs `loginctl enable-linger` so it starts at boot and not only after you log in. Headless machines usually don't have a keyring, so `login` keeps the key password in the session file and there's nothing to unlock. If you want to go back to the graphical version, just run `autostart` again without `-headless`.

You can disable the autostart by passing `-remove` as a flag. This will disable the unit and delete the unit file from the `~/.config/systemd/user/` dir. If you installed this as a package, the unit will stay installed, just disabled. We do not remove the lingering because there's no way to know if your other units need it, so if you want it off you need to run `loginctl disable-linger` yourself.

## make restart

```sh
make restart MP=<mountpoint>
```

You need to pass `MP`. This unmounts the mountpoint, rebuilds the binary, and mounts it again. Use it after you change the code or after a `git pull`, so the running daemon matches the binary on disk.
