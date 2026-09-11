# Configuration

Every flag that `login`, `mount`, and `tray` accept has a matching key in a TOML config file at `$XDG_CONFIG_HOME/proton-drive-fs/config.toml` (falls back to `~/.config/proton-drive-fs/config.toml`), or wherever `-config <path>` points. `status` also reads it for `mountpoint` when none is given on the command line.

The first command you run creates that file if it doesn't exist, with every key commented out at its default value, so you always have a config.toml to edit. An existing file is never overwritten, and if the directory can't be written to, the built-in defaults are used instead.

## Precedence

Values resolve in one direction: a flag on the command line always wins over the config file, and the config file always wins over the built-in default.

## config init and config show

```
proton-drive-fs config init [-config path] [-force]
proton-drive-fs config show [-config path] [flags...]
```

`config init` writes a fully commented config file with every key at its default value and a one-line explanation. Uncomment a line to set it. It refuses to overwrite an existing file unless `-force` is passed. Since every command already creates that same file when it's missing, `config init` is mostly useful with `-force` to reset an edited file, or with `-config` to write one somewhere else.

`config show` prints the effective configuration after merging defaults, the file, and any flags you pass to `config show` itself. Each value has a trailing comment naming where it came from (`default`, `file`, or `flag`, plus `unset` for `mountpoint` alone, which has no built-in default). Useful to check what `mount` or `login` would actually resolve to before running them.

## Keys

| Key | Flag | Default | Meaning |
| --- | --- | --- | --- |
| `mountpoint` | positional for `mount`/`unmount`/`status`, `-mountpoint` for `tray` | none, required | Mountpoint `mount`, `unmount`, `status`, and `tray` fall back to when none is given on the command line. There is no default; see [Indexer spam](troubleshooting.md#indexer-spam) when choosing one. |
| `ttl` | `-ttl` | `30s` | How long a directory listing stays cached before it is fetched again. |
| `poll` | `-poll` | `10s` | How often the event feed is polled for remote changes. |
| `op_timeout` | `-op-timeout` | `60s` | Deadline for one filesystem operation's network calls. |
| `cache_dir` | `-cache-dir` | `$XDG_CACHE_HOME/proton-drive-fs` | Where downloaded file blocks and persisted directory listings are stored on disk. |
| `cache_size` | `-cache-size` | `2GiB` | Total size the on-disk cache (blocks and listings together) may use; `"0"` disables both. |
| `large_file` | `-large-file` | `300MiB` | Files larger than this bypass the on-disk block cache; `"0"` disables the threshold. |
| `thumbnails` | `-thumbnails` | `true` | Write preview images into the freedesktop thumbnail cache (Proton's stored previews and system thumbnailers). |
| `thumbnail_dir` | `-thumbnail-dir` | `$XDG_CACHE_HOME/thumbnails` | Freedesktop thumbnail cache directory. |
| `deny_readers` | `-deny-readers` | see [Usage](usage.md#mount) | Process names refused a read of a file above `large_file`; empty allows all. |
| `max_uploads` | `-max-uploads` | `5` | How many files upload at once. |
| `max_downloads` | `-max-downloads` | `8` | How many file blocks download at once. |
| `log_level` | `-log-level` | `info` | Log verbosity: `debug`, `info`, `warn`, or `error`. |
| `log_stderr` | `-log-stderr` | `false` | Force logging to stderr instead of the systemd journal. |
| `foreground` | `-foreground` | `false` | Stay attached to the terminal instead of detaching into the background. |
| `hv_method` | `-hv-method` | (none) | Force a human verification method at login: `captcha`, `email`, or `sms`. |
| `no_browser` | `-no-browser` | `false` | Do not open a browser for human verification at login. |

See [Usage](usage.md) for the full description of each flag.

## Cache directory

The persisted listing cache stores decrypted file and folder names on disk (under `cache_dir/listings/`, mode 0600) so a folder listed once loads instantly on the next cold start. It shares `cache_size`'s byte budget with the block cache (under `cache_dir/blocks/`). Set `cache_size = "0"` to disable both. See [General table of locations](troubleshooting.md#general-table-of-locations) for the exact paths and [Read the logs](troubleshooting.md#read-the-logs) for cache hit and miss log levels.
