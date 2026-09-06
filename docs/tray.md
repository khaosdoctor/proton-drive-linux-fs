# Tray

```
proton-drive-fs tray [-mountpoint ~/ProtonDrive]
```

Runs a status icon in the system tray over StatusNotifierItem, which is what Waybar,
KDE Plasma, and the GNOME AppIndicator extension speak. With no `-mountpoint` the tray
reuses the last mountpoint it was given and falls back to `~/ProtonDrive`; the choice
is stored in `$XDG_CONFIG_HOME/proton-drive-fs/tray.json` so the menu keeps working
after a restart.

## Icon states

The icon is a cloud in one of four states, checked in this order:

1. **Hollow outline**: no saved session, or nothing mounted. The status line in the
   menu says which of the two it is.
2. **Two bars in the corner**: polling is paused.
3. **A dot in the corner**: a download or an upload is in flight.
4. **Solid**: mounted, logged in, nothing moving.

Each check short-circuits the next: being logged out hides everything else, a pause
marker outranks activity, and a transfer only counts while the mount's status snapshot
is fresh enough to trust.

```mermaid
stateDiagram-v2
    [*] --> NotLoggedIn: no saved session
    [*] --> Paused: logged in, polling paused
    [*] --> NotMounted: logged in, not paused, nothing mounted
    [*] --> Syncing: mounted, not paused, transfer in flight
    [*] --> Online: mounted, not paused, nothing moving

    NotLoggedIn: Hollow outline
    NotMounted: Hollow outline
    Paused: Two bars in the corner
    Syncing: A dot in the corner
    Online: Solid
```

While uploads are queued the status line counts them, as in
`Mounted at ~/ProtonDrive, syncing 312/10000`, and appends `, N failed` when some of
them could not be uploaded. The counts go back to zero half a minute after the queue
drains.

## Menu

The menu holds a status line (`Mounted at <path>`, `Not mounted`, or `Not logged in`),
then items shown only when they apply:

- `Mount` when logged in but not mounted.
- `Unmount` and `Restart mount` when mounted.
- `Pause syncing` or `Resume syncing` when mounted.
- `Open folder` when mounted.
- `Open logs` and `Open debug logs`.
- `Log in` when logged out.
- `Log out` when logged in.
- `Quit`.

`Mount` and `Unmount` run this same binary, so a mount started from the menu is the
same detached mount you get from a shell, and it survives the tray closing. `Quit`
only closes the icon; it never unmounts.

When the status line gets a ` (daemon X, restart needed)` suffix, the running daemon
is an older build than the tray itself, usually because a rebuild's earlier unmount
failed as busy. Click `Restart mount` to unmount and remount with the current binary.

`Log in` needs a terminal because it prompts for the password. The tray starts the
first terminal it finds on `PATH`, trying `$TERMINAL` first and then
`x-terminal-emulator`, `kitty`, `alacritty`, `foot`, `gnome-terminal`, `konsole`,
`xterm`. When none of them is installed, the status line shows the command to run
yourself for ten seconds. `Open logs` follows the journal in a terminal when the mount
logs there, and otherwise opens the log file with `xdg-open`. `Open debug logs` does
the same at debug verbosity (`journalctl --user -t proton-drive-fs -p debug -f`); see
[Logs](troubleshooting.md#logs) for what shows up at each level.

## Pause semantics

Pause stops one thing: the poll of Proton's event feed. Remote changes stop reaching
the mount until you resume, while reads and writes keep working throughout. It is a
marker file at `$XDG_RUNTIME_DIR/proton-drive-fs/paused` (falling back to
`$XDG_STATE_HOME/proton-drive-fs/paused`) that the mount checks on every poll tick, so
it also applies to a mount the tray did not start.

For the syncing state the mount writes `$XDG_RUNTIME_DIR/proton-drive-fs/status.json`
(same fallback) once a second with its pid, version, and the number of transfers in
flight. A snapshot older than ten seconds counts as no mount running; whether the
filesystem is mounted always comes from `/proc/self/mounts` instead.

Reading the status and setting the pause marker both go through the same local API
first, falling back to the status file or the pause marker directly when no daemon
answers on the socket, for example an older build without the API.

```mermaid
sequenceDiagram
    participant Tray
    participant Client as api.Client
    participant Daemon as Mount daemon (unix socket)
    participant File as status.json / pause marker

    Tray->>Client: Status() or SetPaused()
    Client->>Daemon: request over the unix socket
    alt daemon answers
        Daemon-->>Client: live status, or pause acknowledged
        Client-->>Tray: result
    else socket unavailable
        Client--xTray: dial fails
        Tray->>File: read status.json, or write the pause marker directly
        File-->>Tray: snapshot (stale after 10s), or marker written
    end
```

## Waybar, KDE, and GNOME

- Waybar: add the `tray` module to `modules-right` and `"tray": {}` to the config.
- KDE Plasma: works with no setup.
- GNOME: needs the AppIndicator and KStatusNotifierItem Support extension. GNOME Shell
  has no tray of its own.

## Desktop entry

```
install -Dm644 contrib/proton-drive-fs.desktop ~/.local/share/applications/proton-drive-fs.desktop
install -Dm644 contrib/icons/proton-drive-fs.png ~/.local/share/icons/hicolor/64x64/apps/proton-drive-fs.png
```

`make install` does both of these for you.

## Tray unit

To start the tray with the graphical session instead of by hand, use the
`proton-drive-fs-tray.service` user unit described in [Usage](usage.md#systemd-user-units).

## Restart-needed hint

The tray and the mount daemon are separate processes and can end up on different
binary versions after a rebuild, most often when an earlier unmount failed as busy and
left the old daemon running. The status line's ` (daemon X, restart needed)` suffix and
the `Restart mount` menu item exist for exactly this case: they unmount the stale
daemon and mount again with the binary currently on disk.
