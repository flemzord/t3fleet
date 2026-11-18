#!/bin/sh
# Collect licenses from the Go toolchain and modules linked in release binaries.
set -eu

export LC_ALL=C
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Keep these targets in sync with .goreleaser.yaml. Resolve each target so
# platform-specific imports are included as well.
for os in darwin linux; do
    for arch in amd64 arm64; do
        GOOS=$os GOARCH=$arch CGO_ENABLED=0 go list -deps \
            -f '{{with .Module}}{{if not .Main}}{{.Path}}{{"\t"}}{{.Version}}{{"\t"}}{{.Dir}}{{end}}{{end}}' \
            ./cmd/t3fleet >> "$tmp/imports"
    done
done
sort -u "$tmp/imports" > "$tmp/modules"

emit_licenses() {
    label=$1
    dir=$2
    found=
    for file in "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/COPYING* "$dir"/NOTICE* "$dir"/PATENTS "$dir"/COPYRIGHT*; do
        [ -f "$file" ] || continue
        case "${file##*/}" in
            LICENSE*|LICENCE*|COPYING*) found=yes ;;
        esac
        printf '\n=== %s / %s ===\n\n' "$label" "${file##*/}"
        cat "$file"
        printf '\n'
    done
    if [ -z "$found" ]; then
        printf 'No license file for %s\n' "$label" >&2
        return 1
    fi
}

goroot=$(go env GOROOT)
goversion=$(go env GOVERSION)
awk '/^# / { print $2 }' "$goroot/src/vendor/modules.txt" | sort -u > "$tmp/stdlib-vendor"
tab=$(printf '\t')
{
    printf 'Third-party licenses for t3fleet release binaries\n'
    emit_licenses "Go standard library ($goversion)" "$goroot"
    while IFS= read -r module; do
        emit_licenses "$module (bundled in $goversion)" "$goroot/src/vendor/$module"
    done < "$tmp/stdlib-vendor"
    while IFS="$tab" read -r module version dir; do
        [ -n "$module" ] || continue
        emit_licenses "$module $version" "$dir"
    done < "$tmp/modules"
} > "$tmp/THIRD_PARTY_LICENSES.txt"
mv "$tmp/THIRD_PARTY_LICENSES.txt" THIRD_PARTY_LICENSES.txt
