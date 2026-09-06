# How it works

## Three layers

**Auth.** Logs in against Proton's API, handling two-factor codes and human
verification challenges. It derives the account's key password from the login
password and Proton's stored salts, then persists everything needed to restore the
session, including that key password, to the local session file or the OS keyring.

**Drive.** Wraps Proton's Drive API into a tree of nodes (files and folders), exposing
operations to list children, open a file for streamed reading, upload a new revision,
create a folder, move, and trash. Every file and folder name and every byte of file
content is end-to-end encrypted with the account's keys; the drive layer decrypts on
the way in and encrypts on the way out. It also polls Proton's event feed and turns
raw events into a normalized stream the FUSE layer reacts to.

**FUSE.** Publishes that tree as a mounted filesystem with go-fuse. Directory listings
are cached per folder for the configured TTL (`-ttl`) and refetched on expiry or on a
matching remote event.

## Reading a file

Opening a file for reading does not download it in full. The FUSE layer streams and
caches the file's content in fixed 4 MiB blocks, fetching only the blocks a read
actually touches. Reading the first few kilobytes of a large video, for example,
downloads one block, not the whole file.

Downloaded blocks are kept in an on-disk cache (`-cache-dir`, sized by `-cache-size`)
so a second read of the same block, even after a remount, does not cost another
download. Files larger than `-large-file` are still read block by block, but their
blocks skip the on-disk cache: one very large file streamed once should not push
everything else out of a cache sized for everyday use.

## Writing a file

Opening a file for writing buffers the new content to a local temp file. Nothing is
sent to Proton until the file closes, at which point the whole buffered file uploads
as a new revision. There is no partial write to the remote file and no streaming
upload while the file is still open.

## Staying in sync

The mount polls Proton's event feed on the interval set by `-poll`. Each event names
what changed remotely; the FUSE layer uses that to invalidate the affected directory
listing and any cached blocks for a changed file, so the next read or listing picks up
the current state instead of serving something stale. Pausing the tray's sync stops
this poll only; reads and writes you make locally keep working.

## Previews

Proton stores a small thumbnail next to each file it has one for. When a folder is
listed, the mount downloads those thumbnails in the background and writes them into
the freedesktop thumbnail cache, the shared directory file managers read to show
previews without opening the files themselves.

## Reader denylist

Some desktops run thumbnailers and search indexers that open every file in a folder to
inspect it. On a network filesystem that means downloading a file's full content just
to generate a preview or index it. The processes named in `-deny-readers` are refused
a read of any file above `-large-file`; the open fails with a permission error and
nothing downloads. Applications a user launches by hand to open a file are not on the
list and are unaffected.
