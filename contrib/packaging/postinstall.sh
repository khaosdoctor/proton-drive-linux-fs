#!/bin/sh
# Runs after the package installs (fresh or upgrade).
set -e

reload_and_restart() {
  uid=$(id -u "$1")
  export XDG_RUNTIME_DIR="/run/user/$uid"
  export DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
  su - "$1" -c 'systemctl --user daemon-reload' 2>/dev/null || true
  for svc in proton-drive-fs proton-drive-fs-tray; do
    su - "$1" -c "systemctl --user is-active --quiet $svc" 2>/dev/null &&
      su - "$1" -c "systemctl --user restart $svc" 2>/dev/null &&
      echo "Restarted $svc for user $1"
  done
}

# Restart running services for logged-in users
if [ -d /run/user ]; then
  for dir in /run/user/*/; do
    uid=$(basename "$dir")
    user=$(id -nu "$uid" 2>/dev/null) || continue
    reload_and_restart "$user"
  done
fi

cat <<'EOF'
proton-drive-fs installed.

If the systemd user units are not yet enabled:

  systemctl --user enable --now proton-drive-fs-tray
  systemctl --user enable --now proton-drive-fs

Log in first with: proton-drive-fs login
EOF
