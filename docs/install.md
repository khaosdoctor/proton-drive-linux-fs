# Install

## Requirements

- Linux, with FUSE 3 (the `fuse3` package on most distributions) and access to
  `/dev/fuse`.
- Optional: a Secret Service provider (GNOME Keyring, KWallet) to store the drive's
  key password in the OS keyring instead of the session file.
- Optional: `zenity`, used by some desktops for graphical prompts.
- Optional: a running systemd journal, so the mount daemon logs there instead of a
  plain log file.

## From a GitHub Release

```
tar -xzf proton-drive-fs_linux_amd64.tar.gz
sudo install -m 755 proton-drive-fs /usr/local/bin/proton-drive-fs
```

Releases are published on the
[releases page](https://github.com/khaosdoctor/proton-drive-linux-fs/releases), one
tarball per architecture (`amd64`, `arm64`).

## With Go

```
go install github.com/khaosdoctor/proton-drive-linux-fs/cmd/proton-drive-fs@latest
```

## From source

```
git clone https://github.com/khaosdoctor/proton-drive-linux-fs
cd proton-drive-linux-fs
make build
make install
```

`make build` places the binary in the repository root. `make install` copies it to
`$PREFIX/bin` (default `$HOME/.local/bin`), installs the desktop entry and icon, and
installs the two systemd user units described in [Usage](usage.md). Run `make help`
to see every target.

## Native packages

`.deb`, `.rpm`, `.apk`, and Arch packages are attached to each
[release](https://github.com/khaosdoctor/proton-drive-linux-fs/releases). Install one
with the matching package manager (`dpkg`, `rpm`, `apk`, or `pacman`), then enable the
tray with `systemctl --user enable --now proton-drive-fs-tray`.

## Arch Linux (AUR)

```
yay -S proton-drive-fs-bin
```

That installs a prebuilt binary. To build from source instead:

```
yay -S proton-drive-fs
```

Both packages pull in `fuse3` as a dependency and enable the tray the same way
as the native packages above.

### Maintainer note: publishing to the AUR

The release workflow pushes both AUR packages automatically, but the AUR
account and the repo secret that authorizes it are set up once, by hand:

1. Create an account on [aur.archlinux.org](https://aur.archlinux.org) and add
   an SSH public key to it.
2. Add the matching private key as the `AUR_KEY` secret on the GitHub repo:
   `gh secret set AUR_KEY < ~/.ssh/aur`.

The first release after that claims both `proton-drive-fs-bin` and
`proton-drive-fs` on the AUR. Every release after that updates them
automatically. Until `AUR_KEY` is set, the release workflow still runs; it
just skips the AUR publish step instead of failing.

## Container image

```
docker pull ghcr.io/khaosdoctor/proton-drive-linux-fs:latest
```

Run `login` first to create a session, then `mount`:

```
docker run --rm -it \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor:unconfined \
  -v ~/.config/proton-drive-fs:/root/.config/proton-drive-fs \
  -v ~/ProtonDrive:/mnt/protondrive:rshared \
  ghcr.io/khaosdoctor/proton-drive-linux-fs:latest \
  mount /mnt/protondrive
```

`--device /dev/fuse`, `--cap-add SYS_ADMIN`, and `--security-opt apparmor:unconfined`
are what FUSE needs to create a mount inside a container. The bind mount on the config
directory keeps the session across container runs. The bind mount on the mountpoint
needs `:rshared` propagation for the mount created inside the container to become
visible on the host; without it the mount stays confined to the container's own mount
namespace.
