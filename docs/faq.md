# FAQ

**Why unofficial?**

proton-drive-fs talks to Proton's Drive API the same way Proton's own apps do, but it is not written, reviewed, or supported by Proton AG. The API it relies on is not publicly documented and can change without notice.

**Is it a sync client?**

No. There is no background job copying files on a schedule and no local copy of the whole drive. Opening a file downloads it, saving a file uploads it, and a folder listing reflects Proton's current metadata. The only thing running periodically is the event feed poll that keeps the mount aware of remote changes.

**Where is my password stored?**

Your Proton password is never stored. What gets stored is the session Proton issues after login (access and refresh tokens, mode 0600) and the salted key password derived from your account password, which unlocks the drive's encryption keys. The key password goes to the OS keyring when a Secret Service provider is available, otherwise it stays in the session file with mode 0600.

**Why did my file manager stop showing previews for a large file?**

The mount generates thumbnails itself, so thumbnailer processes are always blocked from reading through the FUSE mount. For files below `-large-file` (300MiB by default), the mount downloads the file, runs the matching system thumbnailer, and writes the result into the freedesktop thumbnail cache. Files above that threshold only get a preview when Proton has a stored thumbnail for them. Indexer processes in `-deny-readers` are also blocked above `-large-file`. An application you open a file with directly is not affected. See [How it works](how-it-works.md#previews).

**Does it work in Docker?**

Yes, with `--device /dev/fuse`, `--cap-add SYS_ADMIN`, and `--security-opt apparmor:unconfined`, plus `:rshared` propagation on the mountpoint bind mount so the mount becomes visible on the host. See [Install](install.md#container-image).

**What about shared drives?**

Not supported yet. Only the primary Proton share is mounted; other shares are not exposed.

**Can I use two accounts at once?**

Not directly. The session file holds one account's credentials at a time. A second account would need a separate `$XDG_CONFIG_HOME/proton-drive-fs` directory (for example by setting `XDG_CONFIG_HOME` differently per invocation) and a separate mountpoint.

**Can I write to a file while it is being read elsewhere?**

Only one writer per file at a time. Concurrent writers to the same file are not supported.

**What happens if I edit a huge file?**

The whole file buffers locally while it's open, and the whole file uploads again once it closes and its [upload delay](usage.md#mount) passes. There is no partial or incremental upload, so editing a very large file costs a full re-upload.

**Is there a trash or restore?**

Not yet. Deleting a file removes it from the mount and from Proton; there is no restore path through proton-drive-fs today.

**Why does `mount` refuse to run after I rebuilt the binary?**

`mount` checks whether the mountpoint is already mounted before attaching. If an earlier unmount failed as busy, the old daemon is still serving the mount, and `mount` refuses to start a second one on top of it. See [Stale daemon after a rebuild](troubleshooting.md#stale-daemon-after-a-rebuild).

**Why does `mount` fail with `cannot decode TOML integer into struct field`?**

Durations and sizes in `config.toml` are quoted strings with a unit, the same text you would pass to the flag. Booleans and numbers are not quoted, and lists use brackets even with one entry.

```toml
ttl = 30                # cannot decode TOML integer into struct field config.Config.TTL of type string
ttl = "30"              # time: missing unit in duration "30"
ttl = "30s"             # correct

cache_size = 2147483648 # cannot decode TOML integer into struct field config.Config.CacheSize of type string
cache_size = "2GB"      # invalid size "2GB" (units are K, M, G, KiB, MiB, GiB)
cache_size = "2GiB"     # correct

thumbnails = "false"    # cannot decode TOML string into struct field config.Config.Thumbnails of type bool
thumbnails = false      # correct

max_uploads = "5"       # cannot decode TOML string into struct field config.Config.MaxUploads of type int
max_uploads = 5         # correct

exclude = "*.tmp"       # cannot decode TOML string into struct field config.Config.Exclude of type []string
exclude = ["*.tmp"]     # correct
```

Run `proton-drive-fs config show` after editing the file. It fails with the same error `mount` would, naming the key and, for a type mismatch, the line and column.

**Why does a key I set in `config.toml` have no effect?**

Unknown keys are ignored without an error. Keys use underscores (`upload_delay`, not `upload-delay` as on the command line), the mountpoint key is `mountpoint`, not `mount_point`, and every key goes at the top level of the file. Keys under a section header such as `[mount]` are ignored too. `config show` marks a value read from the file with `# file`; a key you set that still shows `# default` was not read.

**Why are `.DS_Store` and `.~lock` files uploading after I set `exclude`?**

Setting `exclude` or `deny_readers` replaces the default list, it does not add to it. `exclude = ["*.bak"]` excludes `*.bak` and nothing else. Copy the defaults from the commented-out line `config init` writes, then add your own entries to that list.

**Why is my `exclude` or `upload_delays` pattern ignored?**

A broken pattern does not stop the mount. It logs a warning and is skipped, so check the logs (see [Read the logs](troubleshooting.md#read-the-logs)) when a pattern does not match.

```toml
upload_delays = ["*.bak:1m"]        # invalid upload delay rule, want pattern=duration, skipping
upload_delays = ["*.bak=1m"]        # correct

exclude = ['re:^\.~lock\.(']        # invalid exclude regex, skipping
exclude = ['re:^a{1,3}\.tmp$']      # split at the comma into two broken patterns
```

Lists are joined with commas internally, so a pattern cannot contain a comma. Rewrite a regexp like `{1,3}` without one, or use several patterns.

**Why does `config show` accept my `log_level` but `mount` refuses to start?**

`log_level` is only checked when `mount` runs. `log_level = "verbose"` exits with `error: invalid -log-level: unknown level "verbose" (want debug, info, warn or error)`.
