# Configuration

Today, configuration is command-line flags only. There is no environment variable or
file-based configuration for `mount`, `login`, `unmount`, `status`, or `tray` yet.

## Flags

| Flag | Command | Default | Meaning |
|---|---|---|---|
| `-no-browser` | login | `false` | Do not open a browser for human verification. |
| `-hv-method` | login | none forced | Force a verification method: `captcha`, `email`, or `sms`. |
| `-debug` | mount | `false` | Enable FUSE debug logging. |
| `-ttl` | mount | `30s` | Directory listing cache TTL. |
| `-poll` | mount | `10s` | Event feed polling interval. |
| `-op-timeout` | mount | `60s` | Deadline for one filesystem operation's network calls. |
| `-cache-dir` | mount | `$XDG_CACHE_HOME/proton-drive-fs/blocks` | On-disk block cache directory. |
| `-cache-size` | mount | `1GiB` | On-disk block cache size limit. |
| `-large-file` | mount | `300MiB` | Threshold above which blocks bypass the on-disk cache. |
| `-thumbnails` | mount | `true` | Write previews into the freedesktop thumbnail cache. |
| `-thumbnail-dir` | mount | `$XDG_CACHE_HOME/thumbnails` | Thumbnail cache directory. |
| `-deny-readers` | mount | see [Usage](usage.md#mount) | Process names refused a read of a large file. |
| `-max-uploads` | mount | `5` | Concurrent uploads. |
| `-max-downloads` | mount | `8` | Concurrent block downloads. |
| `-foreground` | mount | `false` | Stay attached to the terminal. |
| `-log-level` | mount | `info` | Log verbosity: `debug`, `info`, `warn`, or `error`. |
| `-log-stderr` | mount | `false` | Force logging to stderr instead of the systemd journal. |
| `-force` | unmount | `false` | Lazily unmount and abort the FUSE connection. |
| `-wait` | unmount | `5s` | How long to retry a busy unmount before falling back to lazy. |
| `-mountpoint` | tray | last used, else `~/ProtonDrive` | Mountpoint the tray manages. |

See [Usage](usage.md) for the full description of each flag.

## Configuration file (upcoming)

A `config.toml` file under `$XDG_CONFIG_HOME/proton-drive-fs/` is in progress. It is
not available yet. Once it ships, flags will keep precedence over the file: a flag
passed on the command line overrides whatever the file sets for that same option. This
page will list the file's keys once the feature ships.
