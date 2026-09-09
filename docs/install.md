# Install

## Requirements

- Linux, with FUSE 3 (the `fuse3` package on most distributions) and access to `/dev/fuse`.
- Optional: a Secret Service provider (GNOME Keyring, KWallet) to store the drive's key password in the OS keyring instead of the session file.
- Optional: `zenity`, used by some desktops for graphical prompts.
- Optional: a running systemd journal, otherwise a logfile is used

## Arch Linux (AUR)

The recommended way is to use the prebuilt binary, you can use any AUR helper:

```sh
yay -S proton-drive-fs-bin
```

To build from source instead:

```sh
yay -S proton-drive-fs
```

To build the latest commit on `main`, for unreleased changes:

```sh
yay -S proton-drive-fs-git
```

> Be aware that building from HEAD is __highly experimental__, and it _will_ probably break sometime, so only do that if you're either developing against that branch or you are very bold

All three packages pull in `fuse3` as a dependency and enable the tray the same way as the native packages above.

## Debian / Ubuntu (APT)

Add the repository and install:

```sh
# Import the signing key (skip if the repo is unsigned)
curl -fsSL https://khaosdoctor.github.io/proton-drive-linux-fs/apt/public.key | sudo gpg --dearmor -o /usr/share/keyrings/proton-drive-fs.gpg

# Add the repository
echo "deb [signed-by=/usr/share/keyrings/proton-drive-fs.gpg arch=$(dpkg --print-architecture)] https://khaosdoctor.github.io/proton-drive-linux-fs/apt stable main" | sudo tee /etc/apt/sources.list.d/proton-drive-fs.list

# Install
sudo apt update
sudo apt install proton-drive-fs
```

If the repository is not yet signed (no `public.key` available), use `[trusted=yes]` instead of `[signed-by=...]`.

## Homebrew (Linuxbrew)

```sh
brew tap khaosdoctor/tap
brew install proton-drive-fs
```

## Native packages

`.deb`, `.rpm`, `.apk`, and Arch packages are also attached to each [release](https://github.com/khaosdoctor/proton-drive-linux-fs/releases). Install one with the matching package manager, then enable the tray with `systemctl --user enable --now proton-drive-fs-tray`.

## From a GitHub Release

No package for your distro? Take the raw binary instead, download the tarball from the releases page and install it manually:

```sh
tar -xzf proton-drive-fs_linux_amd64.tar.gz
sudo install -m 755 proton-drive-fs /usr/local/bin/proton-drive-fs
```

## With Go

```sh
go install github.com/khaosdoctor/proton-drive-linux-fs/cmd/proton-drive-fs@latest
```

## From source

Run these in order:

```sh
git clone https://github.com/khaosdoctor/proton-drive-linux-fs
cd proton-drive-linux-fs
make build
make install
```

`make build` places the binary in the repository root. `make install` copies it to `$PREFIX/bin` (default `$HOME/.local/bin`), installs the desktop entry and icon, and installs the two systemd user units described in [Usage](usage.md). Run `make help` to see every target.

## Container image

If you wish to run the proton FUSE drive in a container, we got you covered too:

```sh
docker pull ghcr.io/khaosdoctor/proton-drive-linux-fs:latest
```

Run `login` first to create a session, then `mount`, you can also copy your session file from your local computer to the container via bind mount:

```sh
docker run --rm -it \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --security-opt apparmor:unconfined \
  -v ~/.config/proton-drive-fs:/root/.config/proton-drive-fs \
  -v path/to/mount:/mnt/protondrive:rshared \
  ghcr.io/khaosdoctor/proton-drive-linux-fs:latest \
  mount /mnt/protondrive
```

`--device /dev/fuse`, `--cap-add SYS_ADMIN`, and `--security-opt apparmor:unconfined` are what FUSE needs to create a mount inside a container.

The bind mount on the config directory keeps the session across container runs, and the bind mount on the mountpoint needs `:rshared` propagation for the mount created inside the container to become visible on the host. Without it the mount stays confined to the container's own mount namespace.
