#!/bin/sh
set -e

# dpkg calls "configure <old-version>". The old version is empty on a fresh
# install.
action=${1:-}
old_version=${2:-}

[ "$action" = "configure" ] || exit 0

if command -v systemctl >/dev/null 2>&1; then
    if [ -d /run/systemd/system ]; then
        systemctl daemon-reload || true
    fi
    if [ -z "$old_version" ]; then
        # Fresh install: enable, but do not start. The example configuration
        # points to documentation addresses and needs edits first.
        systemctl enable llm-router >/dev/null 2>&1 || true
    elif [ -d /run/systemd/system ]; then
        # Upgrade: restart only when the service runs.
        systemctl try-restart llm-router || true
    fi
fi

if [ -z "$old_version" ]; then
    echo ""
    echo "llm-router installed. The service is enabled but not started."
    echo "1. Edit /etc/llm-router/config.json and set your model servers."
    echo "2. Run: sudo systemctl start llm-router"
    echo ""
fi
