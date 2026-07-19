#!/bin/sh

set -eu

output=${1:?usage: third-party-licenses.sh OUTPUT}
: >"$output"

append_file() {
	component=$1
	file=$2
	relative=$3
	{
		printf '\n================================================================================\n'
		printf '%s — %s\n' "$component" "$relative"
		printf '================================================================================\n\n'
		cat "$file"
		printf '\n'
	} >>"$output"
}

{
	printf 'THIRD-PARTY LICENSES AND NOTICES\n'
	printf '\nThis file covers components included in the compiled Lazarus container image.\n'
	printf 'Lazarus itself is licensed under Apache-2.0; see /LICENSE.\n'
} >>"$output"

go_root=$(go env GOROOT)
for name in LICENSE PATENTS; do
	file="$go_root/$name"
	test -f "$file" || {
		printf 'missing Go %s file at %s\n' "$name" "$file" >&2
		exit 1
	}
	append_file "Go standard library $(go env GOVERSION)" "$file" "$name"
done

go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}{{end}}' ./cmd/lazarus \
	| LC_ALL=C sort -u \
	| while IFS='|' read -r module version directory; do
		test -n "$module" || continue
		license_files=$(find "$directory" -maxdepth 2 -type f \( \
			-iname 'LICENSE*' -o -iname 'COPYING*' -o -iname 'NOTICE*' -o -iname 'PATENTS*' \
		\) -print | LC_ALL=C sort)
		test -n "$license_files" || {
			printf 'no license file found for %s@%s\n' "$module" "$version" >&2
			exit 1
		}
		printf '%s\n' "$license_files" | while IFS= read -r file; do
			relative=${file#"$directory"/}
			append_file "$module@$version" "$file" "$relative"
		done
	done

ca_notice=/usr/share/doc/ca-certificates/copyright
test -f "$ca_notice" || {
	printf 'missing CA certificate notice at %s\n' "$ca_notice" >&2
	exit 1
}
{
	printf '\n================================================================================\n'
	printf 'Mozilla CA certificate data — MPL-2.0\n'
	printf '================================================================================\n\n'
	sed -n '/^Files: mozilla\/certdata.txt/,$p' "$ca_notice"
	printf '\n'
} >>"$output"
