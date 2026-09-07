# Proton Drive for Linux

> FUSE virtual filesystem for Proton Drive on Linux.

See [docs](https://oss.lsantos.dev/proton-drive-linux-fs/).

My lawyers say I have to write this so:

>Not affiliated with, endorsed by, or supported by Proton AG. Everything here is
>distributed as-is and with no previous warranty.

## What is it

It mounts your Proton Drive as a local folder on Linux using FUSE, because apparently no
one really cares about doing it for this God-forsaken OS, and it was pissing me off.

Files and folders are listed from Proton's metadata and the file is only downloaded when something opens it for performance.

Remote changes reach the mount through Proton's event feed that we keep listening, which invalidates the affected directory listings and cached file content. Writes are buffered to a local temp file and uploaded to Proton in full when the file closes.

The changes are __not__ batched, everything you do is updated file by file, so
it's really not very performance-friendly for a lot of files. But it works.

## Current status

Unofficial. Working. Not super tested for performance or production.

I do use this personally (after all I built it for myself) so I kinda solve what I find, but expect bugs, it's all me and the robot doing this (mostly the robot). Report them on the [issue tracker](https://github.com/khaosdoctor/proton-drive-linux-fs/issues).

## Quick start

Full copy-paste flows are in the [quick start guide](https://oss.lsantos.dev/proton-drive-linux-fs/quickstart/) and full requirements and the container run command are in
the [install docs](https://oss.lsantos.dev/proton-drive-linux-fs/install/).

## Everything else

Check the rest of docs, including usage, architecture, etc on the [website](https://oss.lsantos.dev/proton-drive-linux-fs/).
