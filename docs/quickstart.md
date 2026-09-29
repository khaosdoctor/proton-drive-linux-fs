# Quick start

There are three ways you can run proton-drive-fs, just pick the one you want. If you want every way to get the binary, check [Install](install.md), and for every subcommand and flag, check [Usage](usage.md).

## Installed package

If you're on Arch Linux, the easiest way is through the AUR with any AUR helper:

```sh
yay -S proton-drive-fs-bin
```

If you want to build from source instead:

```sh
yay -S proton-drive-fs
```

Otherwise, download the package for your distro and install it:

```sh
sudo dpkg -i proton-drive-fs_*.deb
```

```sh
sudo rpm -i proton-drive-fs_*.rpm
```

```sh
sudo pacman -U proton-drive-fs_*.pkg.tar.zst
```

```sh
sudo apk add --allow-untrusted proton-drive-fs_*.apk
```

You can also skip the package and install it with Go:

```sh
go install github.com/khaosdoctor/proton-drive-linux-fs/cmd/proton-drive-fs@latest
```

Or build it from source:

```sh
git clone https://github.com/khaosdoctor/proton-drive-linux-fs
cd proton-drive-linux-fs
make build
make install
```

Once you have the `proton-drive-fs` binary, log in once:

```sh
proton-drive-fs login
```

This will ask for your username and password. On the first login Proton usually opens a verification tab in your browser, so solve it there, come back to the terminal and press Enter.

Then mount the drive:

```sh
proton-drive-fs mount some/dir
```

This goes to the background and, when the mount works, you'll see where the logs are and how to unmount:

```
mounted path/to/mount (pid 12345, logs: journalctl --user -t proton-drive-fs); unmount with: proton-drive-fs unmount path/to/mount
```

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

The container image only runs the CLI. You need to log in first so we have a session file, and you need to bind mount the config directory so the session is still there on the next run:

```sh
docker run --rm -it \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor:unconfined \
  -v ~/.config/proton-drive-fs:/root/.config/proton-drive-fs \
  -v path/to/mount:/mnt/protondrive:rshared \
  ghcr.io/khaosdoctor/proton-drive-linux-fs:latest \
  login
```

Then mount it with the same bind mounts:

```sh
docker run --rm -it \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor:unconfined \
  -v ~/.config/proton-drive-fs:/root/.config/proton-drive-fs \
  -v path/to/mount:/mnt/protondrive:rshared \
  ghcr.io/khaosdoctor/proton-drive-linux-fs:latest \
  mount -foreground /mnt/protondrive
```

FUSE needs `--device /dev/fuse`, `--cap-add SYS_ADMIN`, and `--security-opt apparmor:unconfined` to mount inside a container. The mountpoint needs `:rshared` too, otherwise you won't see the mount on your host.

You need `-foreground` here, because if the daemon goes to the background, the container thinks it's done and Docker stops it. There's no journal inside the container, so you can read the logs with:

```sh
docker logs -f <container>
```
