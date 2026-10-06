# Quick start

First get the binary with any method from [Install](install.md). For every subcommand and flag, check [Usage](usage.md).

## Login

Once you have the `proton-drive-fs` binary, log in once:

```sh
proton-drive-fs login
```

This will ask for your username and password. On the first login Proton usually opens a verification tab in your browser, so solve it there, come back to the terminal and press Enter.

## Mount

Then mount the drive:

```sh
proton-drive-fs mount some/dir
```

This goes to the background and, when the mount is working, you'll see where the logs are and how to unmount:

```
mounted path/to/mount (pid 12345, logs: journalctl --user -t proton-drive-fs); unmount with: proton-drive-fs unmount path/to/mount
```

## Commands

If you have a display server, you'll also get a tray icon. You can also run the tray by itself if you want:

```sh
proton-drive-fs tray
```

You can check on it any time:

```sh
proton-drive-fs status
```

If this was a one off thing, you can unmount when you're done. If you want to keep it running, jump to the next section:

```sh
proton-drive-fs unmount path/to/mount
```

So you don't have to type the flags every time, the first command you run creates `$XDG_CONFIG_HOME/proton-drive-fs/config.toml` (or `~/.config/proton-drive-fs/config.toml`) with every setting commented out, so you just uncomment and edit the ones you want. Check [Configuration](configuration.md).

## systemd user units

After you `login`, set your mountpoint in the config file and run:

```sh
proton-drive-fs autostart
```

If you're on a machine without a desktop, use `proton-drive-fs autostart -headless` so it starts at boot. Check [autostart](usage.md#autostart) for more.

You can follow the logs with:

```sh
journalctl --user -u proton-drive-fs -f
```

## Docker

The `login` and `mount` commands for the container image are in [Install](install.md#container-image).
