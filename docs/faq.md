# FAQ

**Why unofficial?**

proton-drive-fs talks to Proton's Drive API the same way Proton's own apps do, but it
is not written, reviewed, or supported by Proton AG. The API it relies on is not
publicly documented and can change without notice.

**Is it a sync client?**

No. There is no background job copying files to or from Proton on a schedule and no
local copy of the whole drive. Opening a file downloads it, saving a file uploads it,
and a folder listing reflects Proton's current metadata. The only thing that runs
periodically is the event feed poll that keeps the mount aware of remote changes.

**Where is my password stored?**

Your Proton password itself is never stored. What gets stored is the session Proton
issues after login (access and refresh tokens, mode 0600) and the salted key password
derived from your account password, which unlocks the drive's encryption keys. That
key password goes to the OS keyring when a Secret Service provider is available, and
otherwise stays in the session file with mode 0600.

**Why did my file manager stop showing previews for a large file?**

Files above `-large-file` (300MiB by default) are off-limits to the dedicated
thumbnailer and indexer processes listed in `-deny-readers`, so those processes cannot
force a full download just to generate a preview. An application you open the file
with directly is not affected. See [How it works](how-it-works.md#reader-denylist).

**Does it work in Docker?**

Yes, with `--device /dev/fuse`, `--cap-add SYS_ADMIN`, and
`--security-opt apparmor:unconfined`, plus `:rshared` propagation on the mountpoint
bind mount so the mount becomes visible on the host. See [Install](install.md#container-image).

**What about shared drives?**

Not supported yet. Only the primary Proton share is mounted; other shares are not
exposed.

**Can I use two accounts at once?**

Not directly. The session file holds one account's credentials at a time, so a second
account would need a separate `$XDG_CONFIG_HOME/proton-drive-fs` directory (for
example by setting `XDG_CONFIG_HOME` differently per invocation) and a separate
mountpoint.

**Can I write to a file while it is being read elsewhere?**

Only one writer per file is supported at a time. Concurrent writers to the same file
are not.

**What happens if I edit a huge file?**

The whole file buffers locally while it is open, and the whole file uploads again when
it closes. There is no partial or incremental upload, so editing a very large file
costs a full re-upload of it.

**Is there a trash or restore?**

Not yet. Deleting a file removes it from the mount and from Proton; there is no
restore path through proton-drive-fs today.

**Why does `mount` refuse to run after I rebuilt the binary?**

`mount` checks whether the mountpoint is already mounted before attaching. If an
earlier unmount failed as busy, the old daemon is still serving the mount, and `mount`
refuses to start a second one on top of it. See
[Stale daemon after a rebuild](troubleshooting.md#stale-daemon-after-a-rebuild).
