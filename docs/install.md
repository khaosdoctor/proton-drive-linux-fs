# Install

## Requirements

- Linux, with FUSE 3 (the `fuse3` package on most distributions) and access to `/dev/fuse`.
- Optional: a Secret Service provider (GNOME Keyring, KWallet) if you want to keep the key password in your OS keyring instead of the session file.
- Optional: `zenity`, some desktops use it for graphical prompts. This uses it for showing dialogs in tray buttons.
- Optional: a running systemd journal, otherwise the logs go to a file (see [Read the logs](troubleshooting.md#read-the-logs)).

## Arch Linux (AUR)

The easiest way is to use the prebuilt binary with any AUR helper:

```sh
yay -S proton-drive-fs-bin
```

If you want to build from source instead:

```sh
yay -S proton-drive-fs
```

If you want the latest commit on `main`, with unreleased changes:

```sh
yay -S proton-drive-fs-git
```

> Be aware that building from HEAD is __highly experimental__, and it _will_ probably break sometime, so only do that if you're either developing against that branch or you are very bold

All three packages already pull `fuse3` as a dependency.

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

If the repository isn't signed yet (there's no `public.key`), use `[trusted=yes]` instead of `[signed-by=...]`.

## Homebrew (Linuxbrew)

```sh
brew tap khaosdoctor/tap
brew install proton-drive-fs
```

## Native packages

You can also get `.deb`, `.rpm`, `.apk`, and Arch packages from each [release](https://github.com/khaosdoctor/proton-drive-linux-fs/releases). Install it with your package manager and run `proton-drive-fs autostart` (or `autostart -headless` if you don't have a desktop).

Every package (AUR, APT, deb, rpm, apk) restarts the `proton-drive-fs` service on upgrade if it's running.

## From a GitHub Release

If there's no package for your distro, you can grab the tarball from the releases page and install the binary yourself:

```sh
tar -xzf proton-drive-fs_linux_amd64.tar.gz
sudo install -m 755 proton-drive-fs /usr/local/bin/proton-drive-fs
```

## With Go

```sh
go install github.com/khaosdoctor/proton-drive-linux-fs/cmd/proton-drive-fs@latest
```

## From source

```sh
git clone https://github.com/khaosdoctor/proton-drive-linux-fs
cd proton-drive-linux-fs
make build
make install
```

`make install` puts the binary in `$PREFIX/bin` (`~/.local/bin` by default) with the desktop entry and icon. If you want it to start with your session, run `proton-drive-fs autostart` after that. You can see every target with `make help`.

## Container image

If you want to run it in a container, I got you covered too:

```sh
docker pull ghcr.io/khaosdoctor/proton-drive-linux-fs:latest
```

The image only runs the CLI. Log in first so there's a session file, and bind mount the config directory so the session is still there on the next run (or so the container uses the session you already have):

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
