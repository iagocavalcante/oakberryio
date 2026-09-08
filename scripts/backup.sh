#!/usr/bin/env bash
# Run as root on the oak box, nightly via oak-backup.timer (see
# deploy/oak-backup.{service,timer}). Idempotent: each run gets its own
# dated directory under the backup root, which lives on the HDD mount
# (/var/lib/oak/backups, see docs/usb.md's storage layout).
set -euo pipefail

DATA_DIR="${OAK_DATA_DIR:-/var/lib/oak}"
BACKUP_ROOT="${OAK_BACKUP_DIR:-/var/lib/oak/backups}"
RETAIN_DAYS="${OAK_BACKUP_RETAIN_DAYS:-7}"

dest="${BACKUP_ROOT}/$(date +%F)"
mkdir -p "${dest}/volumes"

# --sparse preserves the volume files' holes (they're raw sparse disk
# images, see the design doc's Storage section) instead of materializing
# them to their full allocated size.
rsync -a --sparse "${DATA_DIR}/volumes/" "${dest}/volumes/"

# sqlite3 .backup takes an online, consistent snapshot even while oakd has
# oak.db open (unlike a plain file copy).
sqlite3 "${DATA_DIR}/oak.db" ".backup '${dest}/oak.db'"

find "$BACKUP_ROOT" -mindepth 1 -maxdepth 1 -type d -mtime "+${RETAIN_DAYS}" -exec rm -rf {} +

echo "backup complete: ${dest}"
