#!/bin/sh
# Runs after the package installs. Only prints instructions; it never
# enables or starts anything on its own.
set -e

cat <<'EOF'
proton-drive-fs installed.

The systemd user units are not enabled automatically. To enable them:

  systemctl --user enable --now proton-drive-fs-tray
  systemctl --user enable --now proton-drive-fs

Log in first with: proton-drive-fs login
EOF
