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
    for svc in proton-drive-fs proton-drive-fs-tray; do
      su - "$user" -c "systemctl --user is-active --quiet $svc" 2>/dev/null &&
        su - "$user" -c "systemctl --user restart $svc" 2>/dev/null &&
        echo "Restarted $svc for $user"
    done
  done
fi

cat <<'EOF'
proton-drive-fs installed.

If the systemd user units are not yet enabled:

  systemctl --user enable --now proton-drive-fs-tray
  systemctl --user enable --now proton-drive-fs

Log in first with: proton-drive-fs login
EOF
