# Quick start

Everything you need to go from nothing installed to a working mount of your Proton
Drive. See [Install](install.md) for every packaging option and [Usage](usage.md) for
every subcommand and flag.

## 1. Install

On Arch Linux, use the AUR:

```
yay -S proton-drive-fs-bin
```

That pulls a prebuilt binary. To build from source instead, use `proton-drive-fs`
(stable) or `proton-drive-fs-git` (latest commit on `main`, unreleased and
experimental).

On another distro, grab the package for it from the
[releases page](https://github.com/khaosdoctor/proton-drive-linux-fs/releases):

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

No package for your distro? Take the raw binary from the same release:

```
tar -xzf proton-drive-fs_linux_amd64.tar.gz
sudo install -m 755 proton-drive-fs /usr/local/bin/proton-drive-fs
```

Or build it yourself, with Go:

```
go install github.com/khaosdoctor/proton-drive-linux-fs/cmd/proton-drive-fs@latest
```

or from source:

```
git clone https://github.com/khaosdoctor/proton-drive-linux-fs
cd proton-drive-linux-fs
make build
make install
```

See [Install](install.md) for requirements and what each method installs.

## 2. Log in

```
proton-drive-fs login
```

This prompts for your Proton username and password, and a TOTP code if two-factor is
enabled. On first login Proton usually requires human verification (CAPTCHA, an email
code, or an SMS code). By default a browser tab opens for it; solve it there, then come
back to the terminal and press Enter to continue. Pass `-no-browser` to get the
verification URL printed instead and open it yourself, or `-hv-method captcha|email|sms`
to force a specific method.

A successful login writes a session to
`$XDG_CONFIG_HOME/proton-drive-fs/session.json`, so you only do this once.

## 3. Mount your drive

```
proton-drive-fs mount ~/ProtonDrive
```

There is no default mountpoint, so the path is required. If it does not exist, `mount`
creates it. The command detaches into the background once the filesystem is mounted and
prints something like:

```
mounted /home/you/ProtonDrive (pid 12345, logs: journalctl --user -t proton-drive-fs); unmount with: proton-drive-fs unmount /home/you/ProtonDrive
```

Logs go to the systemd journal under the identifier `proton-drive-fs`, readable with
`journalctl --user -t proton-drive-fs`.

## 4. What now?

Open the mountpoint in your file manager. Folders and files show up immediately, listed
from Proton's metadata; a file's content only downloads when you open it. Create, edit,
rename, and delete files the same way you would on any local folder: each change goes to
Proton as it happens, there is no separate sync step and no local copy of the whole
drive.

## 5. Keep it running

You have two ways to run proton-drive-fs day to day.

**Manual.** Run `mount` when you want the drive, `unmount` when you're done:

```
proton-drive-fs mount ~/ProtonDrive
proton-drive-fs unmount ~/ProtonDrive
```

Good for trying it out or for one-off sessions. Nothing keeps it running once you log
out or the process dies.

**systemd (recommended for daily use).** The mount unit reads its mountpoint from
`config.toml` instead of taking one as an argument, so set it first. Running any
`proton-drive-fs` command once creates the file at
`$XDG_CONFIG_HOME/proton-drive-fs/config.toml`; open it and uncomment `mountpoint`:

```
mountpoint = "/home/you/ProtonDrive"
```

Then enable the unit:

```
systemctl --user enable --now proton-drive-fs
```

This mounts at login and on every future login, restarts automatically if the daemon
dies, and logs to the journal as part of the unit. Follow the logs with:

```
journalctl --user -u proton-drive-fs -f
```

`login` still has to run at least once before the unit starts, since it needs a saved
session to work with.

## 6. Tray icon (optional)

A system tray icon shows whether you're logged in, mounted, syncing, or paused, and
lets you mount, unmount, and pause from a menu. Start it by hand:

```
proton-drive-fs tray
```

or keep it running with the graphical session:

```
systemctl --user enable --now proton-drive-fs-tray
```

See [Tray](tray.md) for the icon states and menu.

## 7. Check status / unmount

```
proton-drive-fs status ~/ProtonDrive
proton-drive-fs unmount ~/ProtonDrive
```

Once `mountpoint` is set in `config.toml`, both commands work with no argument.

Running on a headless server or inside a container instead of a desktop? See
[Docker](install.md#container-image) and the [Operation models](usage.md#operation-models)
section of Usage.
