#!/bin/sh
set -e

# The service runs as a dedicated unprivileged account. Keep an existing
# account, for example one created by a manual installation.
if ! getent group llm-router >/dev/null; then
    groupadd --system llm-router
fi
if ! getent passwd llm-router >/dev/null; then
    useradd --system --gid llm-router --home-dir /nonexistent \
        --no-create-home --shell /usr/sbin/nologin \
        --comment "LLM Router" llm-router
fi
