#!/bin/sh
# Runs after the package installs (fresh or upgrade) for deb/rpm/apk.
# Arch packages use the ALPM hook instead.
set -e

# Restart running services for logged-in users.
if [ -x /usr/share/libalpm/scripts/proton-drive-fs-restart ]; then
  /usr/share/libalpm/scripts/proton-drive-fs-restart
elif [ -d /run/user ]; then
  for dir in /run/user/*/; do
    [ -d "$dir" ] || continue
    uid=$(basename "$dir")
    user=$(id -nu "$uid" 2>/dev/null) || continue
    export XDG_RUNTIME_DIR="$dir"
    export DBUS_SESSION_BUS_ADDRESS="unix:path=${dir}bus"
    su - "$user" -c 'systemctl --user daemon-reload' 2>/dev/null || true
    # Move the enable symlink to the unit's current [Install] target. Headless users have no
    # graphical session and keep their default.target link, which reenable would remove.
    su - "$user" -c "systemctl --user is-active --quiet graphical-session.target && systemctl --user is-enabled --quiet proton-drive-fs && systemctl --user reenable --quiet proton-drive-fs" 2>/dev/null || true
    su - "$user" -c "systemctl --user is-active --quiet proton-drive-fs" 2>/dev/null &&
      su - "$user" -c "systemctl --user restart proton-drive-fs" 2>/dev/null &&
      echo "Restarted proton-drive-fs for $user"
    # Remove the legacy tray service unconditionally (may be enabled but not running).
    su - "$user" -c "systemctl --user disable --now --quiet proton-drive-fs-tray" 2>/dev/null || true
  done
fi

cat <<'EOF'
proton-drive-fs installed.

To start it with your session (or at boot with -headless):

  proton-drive-fs autostart

Log in first with: proton-drive-fs login
EOF
