#!/bin/sh
set -eu

if [ "$#" -ne 5 ]; then
	echo "usage: $0 <ref-type> <ref-name> <run-number> <run-attempt> <sha>" >&2
	exit 2
fi

ref_type=$1
ref_name=$2
run_number=$3
run_attempt=$4
sha=$5

if [ "$ref_type" = "tag" ]; then
	version=${ref_name#v}
	case "$version" in
		[0-9]*) ;;
		*) echo "invalid tag version: $ref_name" >&2; exit 2 ;;
	esac
	case "$version" in
		*[!0-9A-Za-z.+~_-]*)
			echo "invalid tag version: $ref_name" >&2
			exit 2
			;;
	esac
	printf '%s\n' "$version"
	exit
fi

case "$run_number$run_attempt" in
	""|*[!0-9]*) echo "run number and attempt must be numeric" >&2; exit 2 ;;
esac
case "$sha" in
	""|*[!0-9A-Fa-f]*) echo "sha must be hexadecimal" >&2; exit 2 ;;
esac

printf '0.0.0~git%s.%s.g%.12s\n' "$run_number" "$run_attempt" "$sha"
