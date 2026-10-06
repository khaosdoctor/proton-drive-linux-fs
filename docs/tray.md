# Tray

```
proton-drive-fs tray [-mountpoint <path>]
```

This puts a status icon in your system tray using StatusNotifierItem, which is what Waybar, KDE Plasma, and the GNOME AppIndicator extension (and most of the others) understand.

If you don't pass `-mountpoint`, the tray uses the `mountpoint` from the config file, and if there's none there, the one it used last time. It saves that and its other state in `$XDG_CONFIG_HOME/proton-drive-fs/tray.json`.

If it can't find any of the three, the tray still starts, but you can't manage the mount from it until you set one (check [No mountpoint configured](#no-mountpoint-configured)).

## Icon states

The icon is a cloud, I don't have a lot of design skills:

| Icon | Meaning |
|---|---|
| Hollow outline | Not logged in, or not mounted |
| Two bars | Polling is paused |
| A dot | A download or upload is in flight |
| Solid | Mounted and idle |

While there are uploads in the queue, the status line counts them, like `Mounted at path/to/mount, syncing 312/10000`, and adds `, N failed` when some of them couldn't be uploaded. The counts go back to zero half a minute after the queue is empty.

## Menu

If you right-click the icon a menu is shown. This menu has a status line (`Mounted at <path>`, `Not mounted`, `Not logged in`, or `No mountpoint configured`), then only the items that apply right now. The menu items are pretty self explanatory.

## No mountpoint configured

The status line will be `No mountpoint configured`, and mount-related items stay hidden. You can fix it with `Open config folder` and setting the mount in the config, or by restarting the tray with `-mountpoint <path>`.

`Log in` needs a terminal because it asks for your password. The tray opens the first terminal it finds on your `PATH`, trying `$TERMINAL` first and then `x-terminal-emulator`, `kitty`, `alacritty`, `foot`, `gnome-terminal`, `konsole`, `xterm`. If you have none of them, the status line shows the command for you to run yourself for ten seconds.

## Pause semantics

Pause stops the poll of Proton's event feed __only__, this means that _remote changes_ stop reaching the mount until you resume, but reads and writes _keep working_.

The paused state is basically a marker file at `$XDG_RUNTIME_DIR/proton-drive-fs/paused` (falling back to `$XDG_STATE_HOME/proton-drive-fs/paused`) that the mount checks on every poll tick.

## Waybar, KDE, and GNOME

- Waybar: add the `tray` module to `modules-right` and `"tray": {}` to the config.
- KDE Plasma: works with no setup.
- GNOME: needs the AppIndicator and KStatusNotifierItem Support extension.
