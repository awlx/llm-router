#!/bin/sh
# Install, upgrade, and remove the deb on a host that runs systemd.
# Usage: sudo ./packaging/test-deb-install.sh <old.deb> <new.deb>
set -eu

old=$1
new=$2
config=/etc/llm-router/config.json
example=/usr/share/llm-router/config.example.json
port=1234

fail() {
	echo "deb install test failed: $*" >&2
	systemctl status llm-router --no-pager >&2 || true
	journalctl -u llm-router --no-pager -n 50 >&2 || true
	exit 1
}

wait_health() {
	i=0
	until curl -fsS "http://127.0.0.1:$1/health" >/dev/null 2>&1; do
		i=$((i + 1))
		[ "$i" -lt 30 ] || fail "router does not answer on port $1"
		sleep 1
	done
}

main_pid() {
	systemctl show -p MainPID --value llm-router
}

export DEBIAN_FRONTEND=noninteractive

# Fresh install: the example configuration is in place. The service is
# enabled, but not started.
dpkg -i "$old"
getent passwd llm-router >/dev/null || fail "system user missing"
[ "$(getent passwd llm-router | cut -d: -f7)" = "/usr/sbin/nologin" ] ||
	fail "system user has a login shell"
test -x /usr/bin/llm-router || fail "binary missing"
/usr/bin/llm-router -version >/dev/null || fail "llm-router -version failed"
cmp -s "$example" "$config" || fail "$config is not the example configuration"
dpkg-query -W -f='${Conffiles}\n' llm-router | grep -q " $config " ||
	fail "$config is not a conffile"
[ -z "$(dpkg -V llm-router)" ] || fail "dpkg -V reports changes"
systemctl is-enabled --quiet llm-router || fail "service not enabled"
! systemctl is-active --quiet llm-router || fail "service started on fresh install"

# The service starts with the unchanged example configuration.
systemctl start llm-router
wait_health "$port"
[ "$(ps -o user= -p "$(main_pid)")" = "llm-router" ] ||
	fail "service does not run as llm-router"
curl -fsS "http://127.0.0.1:$port/metrics" | grep -q '^# HELP' ||
	fail "metrics endpoint does not answer"

# Change the configuration like an administrator.
port=18234
sed -i "s/127.0.0.1:1234/127.0.0.1:$port/" "$config"
systemctl restart llm-router
wait_health "$port"
before=$(sha256sum "$config")
old_pid=$(main_pid)

# Upgrade keeps the changed configuration without a prompt and restarts the
# running service.
dpkg -i "$new" </dev/null
[ "$(sha256sum "$config")" = "$before" ] || fail "upgrade changed $config"
systemctl is-enabled --quiet llm-router || fail "upgrade disabled the service"
wait_health "$port"
[ "$(main_pid)" != "$old_pid" ] || fail "upgrade did not restart the service"

# Upgrade does not start a stopped service.
systemctl stop llm-router
dpkg -i "$new" </dev/null
! systemctl is-active --quiet llm-router || fail "reinstall started a stopped service"
systemctl start llm-router
wait_health "$port"

# Removal stops the service and keeps the configuration.
dpkg -r llm-router
! systemctl is-active --quiet llm-router || fail "service still active after removal"
test ! -e /usr/bin/llm-router || fail "binary still present after removal"
test ! -e /lib/systemd/system/llm-router.service || fail "unit still present after removal"
[ "$(sha256sum "$config")" = "$before" ] || fail "removal changed $config"

# Purge removes the configuration and the enable link.
dpkg -P llm-router
test ! -e "$config" || fail "purge kept $config"
test ! -e /etc/systemd/system/multi-user.target.wants/llm-router.service ||
	fail "purge kept the enable link"

echo "deb install test passed"
