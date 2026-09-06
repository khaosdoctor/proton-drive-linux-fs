# proton-drive-linux-fs

A FUSE virtual filesystem that mounts Proton Drive as a local folder on Linux.

## What it does

proton-drive-fs mounts Proton Drive at a directory you choose. Files and folders are
listed from Proton's metadata; the bytes of a file download only when something opens
it. Remote changes reach the mount through Proton's event feed, which invalidates the
affected directory listings and cached file content. Writes are buffered to a local
temp file and uploaded to Proton in full when the file closes.

## What it does not do

There is no background sync job and no local copy of the whole drive. Each action you
take on the mount goes to Proton as it happens: opening a file downloads it, saving a
file uploads it, deleting a file removes it remotely. Nothing runs on a schedule in the
background beyond the event feed poll that keeps the mount aware of remote changes.

## Status

Early and unofficial. Not affiliated with, endorsed by, or supported by Proton AG. The
tested surface is small: basic read, write, create, delete, rename and move operations
on one Proton Drive share. Expect bugs. Report them on the
[issue tracker](https://github.com/khaosdoctor/proton-drive-linux-fs/issues).

## Quick start

Log in once:

```
proton-drive-fs login
```

Mount the drive:

```
proton-drive-fs mount ~/ProtonDrive
```

Run the tray icon to see mount and sync status at a glance:

```
proton-drive-fs tray
```

See [Install](install.md) for the four ways to get the binary onto your machine, and
[Usage](usage.md) for every subcommand and flag.
