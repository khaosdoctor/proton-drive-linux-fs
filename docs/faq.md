# FAQ

**Why unofficial?**

proton-drive-fs talks to Proton's Drive API the same way Proton's own apps do, but Proton AG didn't write it, doesn't review it, and doesn't support it. The API isn't publicly documented either, so it can change at any time.

**Is it a sync client?**

No. There's no background job copying files and no local copy of your whole drive. Opening a file downloads it, saving a file uploads it, and a folder listing shows what Proton has right now. The only thing running all the time is the event feed poll, so the mount knows about remote changes.

**Where is my password stored?**

Your Proton password is never stored. Check [login](usage.md#login) for what we store instead and where.

**Why did my file manager stop showing previews for a large file?**

We only run a system thumbnailer on files smaller than `-large-file` (300MiB by default). Bigger files only get a preview if Proton has a stored thumbnail for them. Check [Previews](how-it-works.md#previews).

**Does it work in Docker?**

Yes. The commands and the flags FUSE needs are in [Install](install.md#container-image).

**What about shared drives?**

Not supported yet. We only mount your main Proton share, other shares don't show up.

**Can I use two accounts at once?**

Not directly. The session file only holds one account at a time. For a second account, you'd need a separate `$XDG_CONFIG_HOME/proton-drive-fs` directory (setting a different `XDG_CONFIG_HOME` for each run, for example) and a separate mountpoint.

**Can I write to a file while it is being read elsewhere?**

You can only have one writer per file at a time. Two apps writing to the same file at once isn't supported.

**What happens if I edit a huge file?**

The whole file is buffered locally while it's open, and the whole thing uploads again after you close it and its [upload delay](usage.md#mount) passes. We don't do partial uploads, so editing a really big file means uploading all of it again.

**Is there a trash or restore?**

Not yet. Deleting a file removes it from the mount and from Proton, and you can't restore it through proton-drive-fs today.

**Why does `mount` refuse to run after I rebuilt the binary?**

The old daemon is probably still serving the mountpoint. Check [Stale daemon after a rebuild](troubleshooting.md#stale-daemon-after-a-rebuild).

**Why does `mount` fail with `cannot decode TOML integer into struct field`?**

In `config.toml`, durations and sizes are strings in quotes with a unit, the same thing you'd pass to the flag. Booleans and numbers don't have quotes, and lists always need brackets, even with one item.

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

Run `proton-drive-fs config show` after you edit the file. It fails with the same error `mount` would, telling you the key and, for a wrong type, the line and column.

**Why does a key I set in `config.toml` have no effect?**

We ignore keys we don't know without any error. Keys use underscores (`upload_delay`, the command line uses `upload-delay`), the mountpoint key is `mountpoint` (with no underscore), and every key goes at the top level of the file. If you put keys under a section like `[mount]`, they're ignored too. `config show` marks values that came from your file with `# file`, so if a key you set still says `# default`, we didn't read it.

**Why are `.DS_Store` and `.~lock` files uploading after I set `exclude`?**

Setting `exclude` or `deny_readers` replaces the default list instead of adding to it. So `exclude = ["*.bak"]` excludes `*.bak` and nothing else. Copy the defaults from the commented-out line `config init` writes, and add your own entries to that list.

**Why is my `exclude` or `upload_delays` pattern ignored?**

A broken pattern doesn't stop the mount, we log a warning and skip it. So if a pattern doesn't match, check the logs ([Read the logs](troubleshooting.md#read-the-logs)).

```toml
upload_delays = ["*.bak:1m"]        # invalid upload delay rule, want pattern=duration, skipping
upload_delays = ["*.bak=1m"]        # correct

exclude = ['re:^\.~lock\.(']        # invalid exclude regex, skipping
exclude = ['re:^a{1,3}\.tmp$']      # split at the comma into two broken patterns
```

Internally we join lists with commas, so a pattern can't have a comma in it. Write a regexp like `{1,3}` without one, or use more than one pattern.

**Why does `config show` accept my `log_level` but `mount` refuses to start?**

We only check `log_level` when `mount` runs. `log_level = "verbose"` exits with `error: invalid -log-level: unknown level "verbose" (want debug, info, warn or error)`.
