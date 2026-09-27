#!/bin/bash
# Native release construction; never tags, uploads, or publishes a release.
set -euo pipefail
source "$(dirname "$0")/common.sh"
check_build_deps
version="$(cat VERSION)"
[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
  echo 'Stable VERSION required.' >&2
  exit 1
}
revision="$(git rev-parse HEAD)"
if [[ "${DATA_MATE_RELEASE_TRIAL:-0}" != 1 ]]; then
  [[ -z "$(git status --porcelain)" && "$(git describe --exact-match --tags HEAD)" == "v$version" ]] || {
    echo 'Release requires a clean v<VERSION> tag.' >&2
    exit 1
  }
fi
output="${DATA_MATE_RELEASE_OUTPUT:-$project_dir/.dist}"
[[ "$output" == /* && ! -e "$output" ]] || {
  echo 'Release output must be a new absolute directory. Move or remove previous output before rebuilding.' >&2
  exit 1
}
umask 077
mkdir -p "$output"
if [[ "$GOOS" == linux ]]; then
  : "${DATA_MATE_LINUX_SIGNING_KEY:?Select the private Linux release signing key file}"
  binary="$output/data-mate_linux_amd64"
  if [[ "${GITHUB_ACTIONS:-}" == true ]]; then
    "$project_dir/scripts/release-linux-build.sh" "$binary" "$version" "$revision"
  else
    [[ "$(getconf GNU_LIBC_VERSION)" == 'glibc 2.39' ]] || {
      echo 'Linux releases require the Ubuntu 24.04/glibc 2.39 build baseline.' >&2
      exit 1
    }
    go build -mod=readonly -trimpath -ldflags "-X main.version=$version -X main.revision=$revision -X main.dirty=false -X main.environment=production" -o "$binary" ./cmd/data-mate
  fi
  chmod 700 "$binary"
  readelf -d "$binary" >"$output/native-libraries.txt"
  if grep -Eq '\((RPATH|RUNPATH)\)' "$output/native-libraries.txt"; then
    echo 'Unexpected native library search path.' >&2
    exit 1
  fi
  awk '/NEEDED/ && $NF !~ /^\[(libc.so.6|libpthread.so.0|libdl.so.2|libm.so.6|librt.so.1|ld-linux-x86-64.so.2)\]$/ {bad=1} END {exit bad}' "$output/native-libraries.txt"
  openssl dgst -sha256 -sign "$DATA_MATE_LINUX_SIGNING_KEY" -out "$binary.sig" "$binary"
  openssl dgst -sha256 -verify "$project_dir/internal/distribution/linux-public.pem" -signature "$binary.sig" "$binary"
  "$binary" __release-metadata --text >"$output/release_linux_amd64.txt"
  cp scripts/install-release.sh "$output/install.sh"
  (cd "$output" && sha256sum install.sh data-mate_linux_amd64 data-mate_linux_amd64.sig release_linux_amd64.txt >SHA256SUMS_linux_amd64)
  printf 'Verified Linux artifacts: %s\n' "$output"
  exit 0
fi
: "${DATA_MATE_SIGNING_IDENTITY:?Select a Developer ID Application identity}"
: "${DATA_MATE_NOTARY_PROFILE:?Select a notarytool Keychain profile}"
export MACOSX_DEPLOYMENT_TARGET=15.0
export CGO_CFLAGS='-O2 -g -mmacosx-version-min=15.0'
export CGO_LDFLAGS='-O2 -g -mmacosx-version-min=15.0'
binary="$output/data-mate_darwin_arm64"
go build -mod=readonly -trimpath -ldflags "-X main.version=$version -X main.revision=$revision -X main.dirty=false -X main.environment=production" -o "$binary" ./cmd/data-mate
chmod 700 "$binary"
/usr/bin/otool -L "$binary" >"$output/native-libraries.txt"
/usr/bin/awk 'NR>1 && $1 !~ /^\/usr\/lib\// && $1 !~ /^\/System\/Library\// {bad=1} END {exit bad}' "$output/native-libraries.txt" || {
  echo 'Non-system native dependency.' >&2
  exit 1
}
/usr/bin/otool -l "$binary" >"$output/load-commands.txt"
/usr/bin/awk '$1=="minos" {found=1; split($2,v,"."); if(v[1]>15 || (v[1]==15 && v[2]>0)) bad=1} END {exit (!found || bad)}' "$output/load-commands.txt" || {
  echo 'Unsupported minimum macOS load command.' >&2
  exit 1
}
/usr/bin/codesign --force --sign "$DATA_MATE_SIGNING_IDENTITY" --identifier io.github.swqa7697.data-mate --options runtime --timestamp "$binary"
/usr/bin/codesign --verify --strict -R '=anchor apple generic and identifier "io.github.swqa7697.data-mate" and certificate leaf[subject.OU] = "JDNRPL924Q" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists' "$binary"
/usr/bin/ditto -c -k --keepParent "$binary" "$output/notarization.zip"
notary_args=(--keychain-profile "$DATA_MATE_NOTARY_PROFILE")
if [[ -n "${DATA_MATE_NOTARY_KEYCHAIN:-}" ]]; then notary_args+=(--keychain "$DATA_MATE_NOTARY_KEYCHAIN"); fi
xcrun notarytool submit "$output/notarization.zip" "${notary_args[@]}" --wait --timeout 30m --output-format json >"$output/notarization.json"
# plutil is supplied by macOS; no jq/Python dependency in release validation.
[[ "$(/usr/bin/plutil -extract status raw -o - "$output/notarization.json")" == Accepted ]] || {
  echo 'Notarization not accepted.' >&2
  exit 1
}
submission="$(/usr/bin/plutil -extract id raw -o - "$output/notarization.json")"
xcrun notarytool log "$submission" "${notary_args[@]}" "$output/notarization-log.json"
/usr/bin/codesign --verify --strict --check-notarization -R '=anchor apple generic and identifier "io.github.swqa7697.data-mate" and certificate leaf[subject.OU] = "JDNRPL924Q" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and notarized' "$binary"
"$binary" __release-metadata --text >"$output/release.txt"
cp scripts/install-release.sh "$output/install.sh"
(cd "$output" && /usr/bin/shasum -a 256 install.sh data-mate_darwin_arm64 release.txt >SHA256SUMS)
rm "$output/notarization.zip"
printf 'Verified artifacts: %s\nThe hosted release workflow runs fresh macOS 15 acceptance before publication.\n' "$output"
