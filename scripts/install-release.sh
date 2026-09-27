#!/bin/bash
# Release bootstrap: definitions precede the only entry call, including when piped.
set -euo pipefail

fail() {
  printf 'Data Mate: %s\n' "$1" >&2
  exit 1
}

# Standard HTTPS downloads honor curl's proxy environment and CA configuration.
fetch() {
  local address="$1" output="$2" limit="$3" head="${4:-false}"
  local curl_args=(--location --fail --proto '=https' --proto-redir '=https' --tlsv1.2 --silent --show-error --connect-timeout 10 --max-time 300 --max-filesize "$limit" --max-redirs 10 --output "$output" --write-out '%{url_effective}')
  if [[ "$head" == true ]]; then curl_args+=(--head); fi
  fetched_url="$(curl "${curl_args[@]}" "$address")" || fail "download failed: $address"
  [[ "$(file_size "$output")" -le "$limit" ]] || fail 'download exceeded size limit'
}

stable_version() { [[ "$1" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; }

metadata() {
  local key value count=0 seen='|' line
  [[ "$(file_size "$stage/$metadata_name")" -le 4096 ]] || fail 'metadata too large'
  [[ "$(LC_ALL=C /usr/bin/tr -d '\12\40-\176' <"$stage/$metadata_name" | /usr/bin/wc -c)" -eq 0 ]] || fail 'invalid metadata encoding'
  [[ "$(/usr/bin/tail -c 1 "$stage/$metadata_name" | /usr/bin/od -An -tu1 | /usr/bin/tr -d ' ')" == 10 ]] || fail 'truncated metadata'
  while IFS= read -r line; do
    [[ "$line" == *=* ]] || fail 'invalid metadata record'
    key="${line%%=*}"
    value="${line#*=}"
    [[ -n "$value" && "$seen" != *"|$key|"* ]] || fail 'duplicate metadata field'
    seen="$seen$key|"
    count=$((count + 1))
    case "$key" in
    format) [[ "$value" == 1 ]] || fail 'unsupported release format' ;;
    version) [[ "$value" == "$version" ]] || fail 'release version mismatch' ;;
    platform) [[ "$value" == "$platform" ]] || fail 'unsupported release platform' ;;
    minimum_macos | minimum_glibc)
      [[ "$key" == "$minimum_key" ]] || fail 'incorrect minimum OS field'
      [[ "$value" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || fail 'invalid minimum OS'
      minimum="$value"
      ;;
    store_schema | inventory_schema) [[ "$value" == 1 ]] || fail 'incompatible release schema' ;;
    *) fail 'unknown metadata field' ;;
    esac
  done <"$stage/$metadata_name"
  [[ "$count" == 6 ]] || fail 'missing metadata fields'
  local host
  if [[ "$platform" == darwin_arm64 ]]; then host="$(/usr/bin/sw_vers -productVersion)"; else
    host="$(/usr/bin/getconf GNU_LIBC_VERSION)"
    [[ "$host" == 'glibc '* ]] || fail 'glibc is required'
    host="${host#glibc }"
  fi
  /usr/bin/awk -v host="$host" -v minimum="$minimum" 'BEGIN {split(host,h,"."); split(minimum,m,"."); for(i=1;i<=3;i++){if(h[i]+0>m[i]+0)exit 0;if(h[i]+0<m[i]+0)exit 1}}' || fail 'OS version is unsupported'
}

verify_checksums() {
  local hash name extra seen='|' count=0
  while read -r hash name extra; do
    [[ "$hash" =~ ^[0-9a-f]{64}$ && -z "$extra" && "$seen" != *"|$name|"* ]] || fail 'invalid checksum manifest'
    case "$name" in install.sh | "$metadata_name" | "$binary_name" | "$signature_name") ;; *) fail 'unknown checksum asset' ;; esac
    seen="$seen$name|"
    count=$((count + 1))
    if [[ "$name" != install.sh || "${1:-false}" == true ]]; then
      [[ "$(sha256_file "$stage/$name")" == "$hash" ]] || fail 'release checksum mismatch'
    fi
  done <"$stage/$sums_name"
  [[ "$count" == "$checksum_count" ]] || fail 'missing checksum assets'
}

# Verify the pinned publisher without imposing an online notarization lookup.
# Release publication checks notarization; macOS retains its own policy checks.
verify_darwin_signature() {
  local candidate="$1"
  local requirement='=anchor apple generic and identifier "io.github.swqa7697.data-mate" and certificate leaf[subject.OU] = "JDNRPL924Q" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists'
  /usr/bin/codesign --verify --strict -R "$requirement" "$candidate" || fail 'publisher signature verification failed'
}

file_size() {
  if [[ "$(uname -s)" == Darwin ]]; then /usr/bin/stat -f '%z' "$1"; else /usr/bin/stat -c '%s' "$1"; fi
}
sha256_file() {
  if [[ "$(uname -s)" == Darwin ]]; then /usr/bin/shasum -a 256 "$1"; else /usr/bin/sha256sum "$1"; fi | /usr/bin/awk '{print $1}'
}
detect_platform() {
  signature_name=''
  checksum_count=3
  case "$(uname -s)/$(uname -m)" in
  Darwin/arm64)
    platform=darwin_arm64
    metadata_name=release.txt
    sums_name=SHA256SUMS
    minimum_key=minimum_macos
    ;;
  Linux/x86_64)
    platform=linux_amd64
    metadata_name=release_linux_amd64.txt
    sums_name=SHA256SUMS_linux_amd64
    minimum_key=minimum_glibc
    signature_name=data-mate_linux_amd64.sig
    checksum_count=4
    ;;
  *) fail 'supported platforms: macOS arm64 and Linux x86_64' ;;
  esac
  binary_name="data-mate_$platform"
}
linux_public_key() {
  cat <<'PUBLIC_KEY'
-----BEGIN PUBLIC KEY-----
MIIBojANBgkqhkiG9w0BAQEFAAOCAY8AMIIBigKCAYEApuRA2G8bdQhbSR+NqTCA
9fU1kDErMrAGpUGzYgMYGZALKd5IS8w2wThMFbXCXm7AaPS1WVxeda/Kw0mfKggl
CYVqb+QibbpSkElWM7Ct/mGTekb/gbAaRMIvaVwEVjcPLIY+6LuADMeBt5zlMPxF
l+HsJUJgXNNQlgzxSn7KrGOGW++i9ufCYcD4t/P/Rjz7SOW5L8Nv5uU5R3lNhtxh
NBfaEvuMd0635sLL4NrpSU9cpcUP+XyeZLU5u/XvNGo545ZeGg5YtSMSVppl5op6
5HkHwvCiOg5z1MX/ZGbidYfKyVjzAQMGJu0eVhaZZ2bYLayO589/OjcUQL6nUmLt
OsK1VCmC/4q1qpnCZP1oqW35AS7Y/JtoDpCeaqzzFwfjs6fj2L+avY3DR/6Fzs40
ozWiBEHni42a8YbWUUkDYRtgKe0Pi+ADHRM/Dn6MHa37vWDUBbECueK11Px3gQzU
V0OhnHHk3mlNZoPex8rDGQXnfDl8lS8IpAV4tUFhMMWzAgMBAAE=
-----END PUBLIC KEY-----
PUBLIC_KEY
}
verify_signature() {
  if [[ "${platform:-}" == '' ]]; then detect_platform; fi
  if [[ "$platform" == darwin_arm64 ]]; then verify_darwin_signature "$1"; else
    /usr/bin/openssl dgst -sha256 -verify <(linux_public_key) -signature "$1.sig" "$1" >/dev/null || fail 'publisher signature verification failed'
  fi
}

main() {
  local no_shell=false uninstall=false purge=false argument stage fetched_url version minimum

  for argument in "$@"; do
    case "$argument" in
    --no-shell) no_shell=true ;;
    --uninstall) uninstall=true ;;
    --purge) purge=true ;;
    *)
      printf 'Invalid installer option.\n' >&2
      exit 2
      ;;
    esac
  done
  if [[ "$purge" == true && "$uninstall" != true ]] || [[ "$uninstall" == true && "$purge" != true ]]; then
    printf 'Bootstrap cleanup requires --uninstall --purge.\n' >&2
    exit 2
  fi
  detect_platform
  umask 077
  stage="$(/usr/bin/mktemp -d /tmp/data-mate-download.XXXXXX)"
  # Capture mktemp's fixed-template pathname now: main's locals are out of
  # scope when errexit runs the EXIT trap. Never interpolate release metadata.
  trap "/bin/rm -rf -- '$stage'" EXIT
  fetch 'https://github.com/swqa7697/data-mate/releases/latest' "$stage/latest" 1048576 true
  case "$fetched_url" in https://github.com/swqa7697/data-mate/releases/tag/v*) version="${fetched_url##*/v}" ;; *) fail 'invalid stable release redirect' ;; esac
  stable_version "$version" || fail 'stable semantic release required'
  local base="https://github.com/swqa7697/data-mate/releases/download/v$version"
  fetch "$base/$metadata_name" "$stage/$metadata_name" 4096
  metadata
  fetch "$base/$sums_name" "$stage/$sums_name" 65536
  fetch "$base/$binary_name" "$stage/$binary_name" 268435456
  if [[ -n "$signature_name" ]]; then fetch "$base/$signature_name" "$stage/$signature_name" 4096; fi
  verify_checksums
  verify_signature "$stage/$binary_name"
  /bin/chmod 700 "$stage/$binary_name"
  "$stage/$binary_name" __release-metadata --text >"$stage/compiled.txt"
  /usr/bin/cmp -s "$stage/$metadata_name" "$stage/compiled.txt" || fail 'compiled release metadata mismatch'
  local args=(__install)
  if [[ "$no_shell" == true ]]; then args+=(--no-shell); fi
  if [[ "$uninstall" == true ]]; then args=(__uninstall --purge); fi
  "$stage/$binary_name" "${args[@]}"
  /bin/rm -rf -- "$stage"
  trap - EXIT
}

# Sourcing exposes these definitions; direct and curl-piped execution
# both enter main (BASH_SOURCE is unset when Bash reads its program from stdin).
if [[ "${BASH_SOURCE[0]:-$0}" == "$0" ]]; then main "$@"; fi
