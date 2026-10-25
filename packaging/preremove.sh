#!/bin/sh
set -e

# On upgrade, the old process continues until postinst restarts it.
if [ "${1:-}" = "remove" ] && command -v systemctl >/dev/null 2>&1 &&
    [ -d /run/systemd/system ]; then
    systemctl stop llm-router 2>/dev/null || true
fi
