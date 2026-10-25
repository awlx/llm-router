#!/bin/sh
set -e

if [ "${1:-}" = "purge" ]; then
    # The unit file is gone, so "systemctl disable" cannot read it.
    rm -f /etc/systemd/system/multi-user.target.wants/llm-router.service
    rmdir /etc/llm-router 2>/dev/null || true
fi

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
