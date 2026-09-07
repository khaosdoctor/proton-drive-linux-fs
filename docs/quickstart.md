# Quick start

Three complete flows, pick the one that matches how you run proton-drive-fs. See [Install](install.md) for every way to get the binary and [Usage](usage.md) for every subcommand and flag.

## Installed package

For Arch Linux, the recommended way is through the AUR, using any AUR helper:

```sh
yay -S proton-drive-fs-bin
```

To build from source instead:

```sh
yay -S proton-drive-fs
```

Otherwise, install the binary. Download the one for your distro:

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

Or without a package, directly through Go:

```sh
go install github.com/khaosdoctor/proton-drive-linux-fs/cmd/proton-drive-fs@latest
```

Or directly build from source:

```sh
git clone https://github.com/khaosdoctor/proton-drive-linux-fs
cd proton-drive-linux-fs
make build
make install
```

Once installed, you will have a `proton-drive-fs` binary and a systemd unit. Log in once:

```sh
proton-drive-fs login
```

This prompts for your username and password. On first login Proton usually opens a verification tab in your browser; solve it there, then come back to the terminal and press Enter to continue.

Mount the drive:

```sh
proton-drive-fs mount some/dir
```

By default this detaches into the background and prints where its logs went, along with the command to unmount when the mount succeeds, you will see this log

```
mounted path/to/mount (pid 12345, logs: journalctl --user -t proton-drive-fs); unmount with: proton-drive-fs unmount path/to/mount
```

Optionally, run the tray icon to see mount and sync status:

```sh
proton-drive-fs tray
```

> You can also run this first, and the use the GUI to do all the previous
> operations

Check on it any time:

```sh
proton-drive-fs status
```

Unmount when done if it's a one off thing. Otherwise jump to the next session to
know how to keep it running:

```sh
proton-drive-fs unmount path/to/mount
```

To avoid having to write the fglags every time, the first command you run writes `$XDG_CONFIG_HOME/proton-drive-fs/config.toml` (falls back to `~/.config/proton-drive-fs/config.toml`) with every setting commented out at its default, so you can uncomment and edit the ones you want. See [Configuration](configuration.md).

## systemd user units

If you installed it with `make install`, the two units are in `~/.config/systemd/user/`. If you installed through a package manager, they are in `/usr/lib/systemd/user/` instead. Either way `login` first (see above), then edit the config file to set your mountpoint, and then enable the mount:

```sh
systemctl --user enable --now proton-drive-fs
```

You can also enable the tray icon alongside it to have a nice little GUI to look
at:

```sh
systemctl --user enable --now proton-drive-fs-tray
```

You can follow logs with:

```sh
journalctl --user -u proton-drive-fs -f
```

## Docker

The container image runs the CLI only. Log in first outside the container (or in a disposable container) so the session file is created, with the config directory bind-mounted so the session survives between runs:

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

Then mount, with the same bind mounts:

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

`--device /dev/fuse`, `--cap-add SYS_ADMIN`, and `--security-opt apparmor:unconfined`
are what FUSE needs to create a mount inside a container. The mountpoint bind needs
`:rshared` propagation for the mount created inside the container to become visible on
the host. Without it the mount stays confined to the container's own mount namespace.

Run `mount` with `-foreground` so the daemon stays attached instead of detaching into the background the way it does outside a container. If you don't, the container's main process exits right after the mount succeeds and Docker stops the container. There is no systemd journal inside the container, so logs go to the container's own stdout/stderr instead. You can read with:

```sh
docker logs -f <container>
```
