# Quick start

Three complete flows, pick the one that matches how you run proton-drive-fs. See
[Install](install.md) for every way to get the binary and [Usage](usage.md) for every
subcommand and flag.

## Installed package (normal flow)

Install the binary. Pick whichever matches how you got it:

```
sudo dpkg -i proton-drive-fs_*.deb
```

```
sudo rpm -i proton-drive-fs_*.rpm
```

```
sudo pacman -U proton-drive-fs_*.pkg.tar.zst
```

```
sudo apk add --allow-untrusted proton-drive-fs_*.apk
```

or without a package:

```
go install github.com/khaosdoctor/proton-drive-linux-fs/cmd/proton-drive-fs@latest
```

```
git clone https://github.com/khaosdoctor/proton-drive-linux-fs
cd proton-drive-linux-fs
make build
make install
```

Log in once:

```
proton-drive-fs login
```

This prompts for your username and password. On first login Proton usually opens a
verification tab in your browser; solve it there, then come back to the terminal and
press Enter to continue.

Mount the drive:

```
proton-drive-fs mount ~/ProtonDrive
```

By default this detaches into the background and prints where its logs went, along
with the command to unmount when the mount succeeds, for example:

```
mounted /home/you/ProtonDrive (pid 12345, logs: journalctl --user -t proton-drive-fs); unmount with: proton-drive-fs unmount /home/you/ProtonDrive
```

Optionally, run the tray icon to see mount and sync status at a glance:

```
proton-drive-fs tray
```

Check on it any time:

```
proton-drive-fs status
```

Unmount when done:

```
proton-drive-fs unmount ~/ProtonDrive
```

Repeating the same flags on every run gets old fast; `proton-drive-fs config init`
writes a `config.toml` with every setting commented out at its default, so you can
uncomment and edit the ones you want instead of passing flags each time. See
[Configuration](configuration.md).

## systemd user units

After `make install`, the two units are in `~/.config/systemd/user/`; after installing
a package, they are in `/usr/lib/systemd/user/` instead. Either way `login` first (see
above), then enable the mount:

```
systemctl --user enable --now proton-drive-fs
```

This mounts `~/ProtonDrive` at login and on every future login; the unit runs
`mount -foreground` so it stays attached and systemd supervises it directly. Enable the
tray icon alongside it:

```
systemctl --user enable --now proton-drive-fs-tray
```

Follow the logs:

```
journalctl --user -u proton-drive-fs -f
```

The unit's mountpoint is fixed at `~/ProtonDrive`. To use a different one, edit the
unit:

```
systemctl --user edit proton-drive-fs
```

and override `ExecStart` with your own mountpoint in the drop-in file that opens.

## Docker

The container image runs the CLI only; there is no tray and no GUI inside it. Log in
first, with the config directory bind-mounted so the session survives between runs:

```
docker run --rm -it \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor:unconfined \
  -v ~/.config/proton-drive-fs:/root/.config/proton-drive-fs \
  -v ~/ProtonDrive:/mnt/protondrive:rshared \
  ghcr.io/khaosdoctor/proton-drive-linux-fs:latest \
  login
```

Then mount, with the same bind mounts:

```
docker run --rm -it \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor:unconfined \
  -v ~/.config/proton-drive-fs:/root/.config/proton-drive-fs \
  -v ~/ProtonDrive:/mnt/protondrive:rshared \
  ghcr.io/khaosdoctor/proton-drive-linux-fs:latest \
  mount -foreground /mnt/protondrive
```

`--device /dev/fuse`, `--cap-add SYS_ADMIN`, and `--security-opt apparmor:unconfined`
are what FUSE needs to create a mount inside a container. The mountpoint bind needs
`:rshared` propagation for the mount created inside the container to become visible on
the host; without it the mount stays confined to the container's own mount namespace.

Run `mount` with `-foreground` so the daemon stays attached instead of detaching into
the background the way it does outside a container; without it the container's main
process exits right after the mount succeeds and Docker stops the container along
with it. There is no systemd journal inside the container, so logs go to the
container's own stdout/stderr instead. Run detached (`-d` instead of `-it`) and read
them with:

```
docker logs -f <container>
```
