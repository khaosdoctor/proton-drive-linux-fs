# Configuration

Every flag you can pass to `login`, `mount`, and `tray` also has a key in a TOML config file. The file lives at `$XDG_CONFIG_HOME/proton-drive-fs/config.toml` (or `~/.config/proton-drive-fs/config.toml`), or wherever you point `-config <path>`. `status` also reads `mountpoint` from it when you don't pass one.

You don't need to create it yourself. The first command you run writes it with every key commented out at its default value, so you just uncomment what you want to change. If we can't write to that directory, we just use the built-in defaults.

We never reset your file to the defaults, but when you upgrade we do edit it to match the keys the new version knows. New keys go at the end, commented out at their default, and keys that don't exist anymore lose their commented-out lines. Anything you set yourself is never touched. The edit goes to a temporary file that's moved into place, and if your config.toml is a symlink, we update the file the link points to. Package upgrades restart the daemon, so this happens on the first start after the upgrade.

## Precedence

A flag on the command line wins over the config file, and the config file wins over the built-in default.

## config init and config show

```
proton-drive-fs config init [-config path] [-force]
proton-drive-fs config show [-config path] [flags...]
```

`config init` writes a config file with every key commented out at its default value and a one-line explanation, so you uncomment a line to set it. It won't overwrite a file that already exists unless you pass `-force`. Since every command already creates that file when it's missing, you only really need `config init` with `-force` to reset a file you edited, or with `-config` to write one somewhere else.

`config show` prints the configuration you'd actually get after merging the defaults, the file, and any flags you pass to `config show` itself. Each value has a comment saying where it came from: `default`, `file`, or `flag` (and `unset` for `mountpoint`, which has no default). It's the easiest way to check what `mount` or `login` would use before you run them.

## Keys

| Key | Flag | Default | Meaning |
| --- | --- | --- | --- |
| `mountpoint` | positional for `mount`/`unmount`/`status`, `-mountpoint` for `tray` | none, required | The mountpoint `mount`, `unmount`, `status`, and `tray` use when you don't pass one. There's no default, so read [Indexer spam](troubleshooting.md#indexer-spam) before you pick one. |
| `ttl` | `-ttl` | `30s` | How long a directory listing stays cached before we fetch it again. |
| `poll` | `-poll` | `10s` | How often we poll the event feed for remote changes. |
| `op_timeout` | `-op-timeout` | `60s` | Deadline for the network calls of one filesystem operation. |
| `cache_dir` | `-cache-dir` | `$XDG_CACHE_HOME/proton-drive-fs` | Where we keep downloaded file blocks and directory listings on disk. |
| `cache_size` | `-cache-size` | `2GiB` | How much space the on-disk cache can use, blocks and listings together. `"0"` turns both off. |
| `large_file` | `-large-file` | `300MiB` | Files bigger than this skip the on-disk block cache. `"0"` turns the threshold off. |
| `thumbnails` | `-thumbnails` | `true` | Write previews into the freedesktop thumbnail cache, from Proton's stored previews and the system thumbnailers. |
| `thumbnail_dir` | `-thumbnail-dir` | `$XDG_CACHE_HOME/thumbnails` | The freedesktop thumbnail cache directory. |
| `deny_readers` | `-deny-readers` | see [Usage](usage.md#mount) | Processes that can't read files bigger than `large_file`. Empty lets everyone read. |
| `exclude` | `-exclude` | see [Usage](usage.md#mount) | Filename patterns we hide from listings and never upload. Globs by default, prefix with `re:` for a regexp. |
| `upload_delay` | `-upload-delay` | `5s` | How long a saved file waits after it closes before we upload it. `"0s"` uploads right on close. |
| `upload_delays` | `-upload-delays` | `[]` | Per-pattern `upload_delay` overrides as `"pattern=duration"`, like `["*.bak=1m"]`. The first pattern that matches wins. |
| `max_uploads` | `-max-uploads` | `5` | How many files we upload at the same time. |
| `max_downloads` | `-max-downloads` | `20` | How many file blocks we download at the same time. |
| `log_level` | `-log-level` | `info` | How much to log: `debug`, `info`, `warn`, or `error`. |
| `log_stderr` | `-log-stderr` | `false` | Log to stderr instead of the systemd journal. |
| `foreground` | `-foreground` | `false` | Stay attached to the terminal instead of going to the background. |
| `hv_method` | `-hv-method` | (none) | Force a human verification method at login: `captcha`, `email`, or `sms`. |
| `no_browser` | `-no-browser` | `false` | Don't open a browser for human verification at login. |

Check [Usage](usage.md) for the full description of each flag.

## Cache directory

The listing cache keeps decrypted file and folder names on disk (under `cache_dir/listings/`, mode 0600), so a folder you listed before shows up right away the next time you mount. It shares the `cache_size` budget with the block cache (under `cache_dir/blocks/`), and `cache_size = "0"` turns both off. The exact paths are in [General table of "where things are at"](troubleshooting.md#general-table-of-where-things-are-at), and [Read the logs](troubleshooting.md#read-the-logs) tells you which log level shows cache hits and misses.
