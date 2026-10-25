#!/bin/sh
# Static checks for the package files. No root and no network needed.
set -eu

repo=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)

fail() {
	echo "packaging assertion failed: $*" >&2
	exit 1
}

assert_contains() {
	grep -F -- "$2" "$1" >/dev/null || fail "$1 does not contain $2"
}

assert_not_contains() {
	if grep -F -- "$2" "$1" >/dev/null; then
		fail "$1 unexpectedly contains $2"
	fi
}

unit=$repo/packaging/llm-router.service
assert_contains "$unit" "ExecStart=/usr/bin/llm-router"
assert_contains "$unit" "Environment=LLM_ROUTER_CONFIG=/etc/llm-router/config.json"
assert_contains "$unit" "ConditionPathExists=/etc/llm-router/config.json"
assert_contains "$unit" "User=llm-router"
assert_contains "$unit" "NoNewPrivileges=true"
assert_contains "$unit" "ProtectSystem=strict"
assert_not_contains "$unit" "/usr/local/"

# The manual-install unit must match the packaged unit, except for the
# binary path and the start condition.
grep -v -e '^ConditionPathExists=' "$unit" |
	sed 's#^ExecStart=/usr/bin/#ExecStart=/usr/local/bin/#' |
	cmp -s - "$repo/llm-router.service" ||
	fail "llm-router.service differs from packaging/llm-router.service"

nfpm=$repo/nfpm.yaml
assert_contains "$nfpm" "dst: /usr/bin/llm-router"
assert_contains "$nfpm" "dst: /lib/systemd/system/llm-router.service"
assert_contains "$nfpm" "dst: /usr/share/llm-router/config.example.json"
assert_contains "$nfpm" "dst: /etc/llm-router/config.json"
assert_contains "$nfpm" "type: config|noreplace"

for script in preinstall postinstall preremove postremove package-version test-packaging test-deb-install; do
	file=$repo/packaging/$script.sh
	[ -x "$file" ] || fail "$file is not executable"
	sh -n "$file" || fail "$file has a syntax error"
done

version=$repo/packaging/package-version.sh
[ "$("$version" tag v1.2.3 1 1 abc)" = "1.2.3" ] || fail "tag version"
[ "$("$version" branch main 42 2 0123456789abcdef)" = "0.0.0~git42.2.g0123456789ab" ] ||
	fail "snapshot version"
if "$version" tag vfoo 1 1 abc 2>/dev/null; then
	fail "accepted tag without numeric version"
fi
if "$version" tag 'v1.0;x' 1 1 abc 2>/dev/null; then
	fail "accepted tag with invalid characters"
fi

assert_contains "$repo/packaging/package-repository-index.html" "llm-router.gpg.key"
echo "packaging checks passed"
