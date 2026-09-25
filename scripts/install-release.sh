#!/bin/bash
# Release bootstrap: definitions precede the only entry call, including when piped.
set -euo pipefail

fail() {
  printf 'Data Mate: %s\n' "$1" >&2
  exit 1
}

allowed_url() {
  case "$1" in
  https://github.com/swqa7697/data-mate/releases/* | https://github.com:443/swqa7697/data-mate/releases/* | https://api.github.com/repos/swqa7697/data-mate/releases/* | https://release-assets.githubusercontent.com/* | https://objects.githubusercontent.com/*) ;;
  *) return 1 ;;
  esac
  [[ "$1" != *'#'* && "$1" != *$'\r'* && "$1" != *$'\n'* ]]
}

# Follow redirects explicitly so curl cannot contact an untrusted redirect host.
fetch() {
  local address="$1" output="$2" limit="$3" head="${4:-false}"
  local code next count=0 started=$SECONDS remaining
  local curl_args=()
  while :; do
    allowed_url "$address" || fail 'untrusted release URL'
    remaining=$((300 - SECONDS + started))
    ((remaining > 0)) || fail 'download timed out'
    curl_args=(--disable --proxy '' --proto '=https' --tlsv1.2 --silent --show-error --connect-timeout 10 --max-time "$remaining" --max-filesize "$limit" --max-redirs 0 --dump-header "$stage/headers" --output "$output" --write-out '%{http_code}')
    if [[ "$head" == true ]]; then curl_args+=(--head); fi
    code="$(/usr/bin/curl "${curl_args[@]}" "$address")" || fail 'download failed'
    [[ "$(/usr/bin/stat -f '%z' "$output")" -le "$limit" ]] || fail 'download exceeded size limit'
    case "$code" in
    200)
      fetched_url="$address"
      return
      ;;
    301 | 302 | 303 | 307 | 308)
      count=$((count + 1))
      ((count <= 5)) || fail 'too many redirects'
      next="$(/usr/bin/awk 'tolower($1)=="location:" {sub(/\r$/, "", $2); print $2}' "$stage/headers")"
      [[ -n "$next" && "$next" != *$'\n'* ]] || fail 'invalid redirect'
      if [[ "$head" != true && "$next" == https://github.com/* && "$next" != "$1" ]]; then fail 'redirect changed the pinned release asset'; fi
      address="$next"
      ;;
    *) fail 'stable release unavailable' ;;
    esac
  done
}

stable_version() { [[ "$1" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; }

metadata() {
  local key value count=0 seen='|' line
  [[ "$(/usr/bin/stat -f '%z' "$stage/release.txt")" -le 4096 ]] || fail 'metadata too large'
  [[ "$(LC_ALL=C /usr/bin/tr -d '\12\40-\176' <"$stage/release.txt" | /usr/bin/wc -c)" -eq 0 ]] || fail 'invalid metadata encoding'
  [[ "$(/usr/bin/tail -c 1 "$stage/release.txt" | /usr/bin/od -An -tu1 | /usr/bin/tr -d ' ')" == 10 ]] || fail 'truncated metadata'
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
    platform) [[ "$value" == darwin_arm64 ]] || fail 'unsupported release platform' ;;
    minimum_macos)
      [[ "$value" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || fail 'invalid minimum OS'
      minimum="$value"
      ;;
    store_schema | inventory_schema) [[ "$value" == 1 ]] || fail 'incompatible release schema' ;;
    *) fail 'unknown metadata field' ;;
    esac
  done <"$stage/release.txt"
  [[ "$count" == 6 ]] || fail 'missing metadata fields'
  /usr/bin/awk -v host="$(/usr/bin/sw_vers -productVersion)" -v minimum="$minimum" 'BEGIN {split(host,h,"."); split(minimum,m,"."); for(i=1;i<=3;i++){if(h[i]+0>m[i]+0)exit 0;if(h[i]+0<m[i]+0)exit 1}}' || fail 'macOS version is unsupported'
}

verify_checksums() {
  local hash name extra seen='|' count=0
  while read -r hash name extra; do
    [[ "$hash" =~ ^[0-9a-f]{64}$ && -z "$extra" && "$seen" != *"|$name|"* ]] || fail 'invalid checksum manifest'
    case "$name" in install.sh | release.txt | data-mate_darwin_arm64) ;; *) fail 'unknown checksum asset' ;; esac
    seen="$seen$name|"
    count=$((count + 1))
    if [[ "$name" != install.sh ]]; then
      [[ "$(/usr/bin/shasum -a 256 "$stage/$name" | /usr/bin/awk '{print $1}')" == "$hash" ]] || fail 'release checksum mismatch'
    fi
  done <"$stage/SHA256SUMS"
  [[ "$count" == 3 ]] || fail 'missing checksum assets'
}

main() {
  local no_shell=false uninstall=false purge=false argument stage fetched_url version minimum
  [[ "$EUID" != 0 ]] || fail 'run as your normal macOS account, not root'
  [[ "$(/usr/bin/uname -s)" == Darwin && "$(/usr/bin/uname -m)" == arm64 ]] || fail 'native macOS arm64 is required'
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
  umask 077
  stage="$(/usr/bin/mktemp -d /private/tmp/data-mate-download.XXXXXX)"
  # The trap stores a literal pathname produced by mktemp, never metadata.
  trap '/bin/rm -rf -- "$stage"' EXIT
  fetch 'https://github.com/swqa7697/data-mate/releases/latest' "$stage/latest" 1048576 true
  case "$fetched_url" in https://github.com/swqa7697/data-mate/releases/tag/v*) version="${fetched_url##*/v}" ;; *) fail 'invalid stable release redirect' ;; esac
  stable_version "$version" || fail 'stable semantic release required'
  local base="https://github.com/swqa7697/data-mate/releases/download/v$version"
  fetch "$base/release.txt" "$stage/release.txt" 4096
  metadata
  fetch "$base/SHA256SUMS" "$stage/SHA256SUMS" 65536
  fetch "$base/data-mate_darwin_arm64" "$stage/data-mate_darwin_arm64" 268435456
  verify_checksums
  case "$(/usr/bin/file -b "$stage/data-mate_darwin_arm64")" in 'Mach-O 64-bit executable arm64'*) ;; *) fail 'native arm64 executable required' ;; esac
  /usr/bin/codesign --verify --strict --check-notarization -R '=anchor apple generic and identifier "io.github.swqa7697.data-mate" and certificate leaf[subject.OU] = "JDNRPL924Q" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and notarized' "$stage/data-mate_darwin_arm64" || fail 'publisher signature or notarization verification failed'
  /bin/chmod 700 "$stage/data-mate_darwin_arm64"
  "$stage/data-mate_darwin_arm64" __release-metadata --text >"$stage/compiled.txt"
  /usr/bin/cmp -s "$stage/release.txt" "$stage/compiled.txt" || fail 'compiled release metadata mismatch'
  local args=(__install)
  if [[ "$no_shell" == true ]]; then args+=(--no-shell); fi
  if [[ "$uninstall" == true ]]; then args=(__uninstall --purge); fi
  "$stage/data-mate_darwin_arm64" "${args[@]}"
  /bin/rm -rf -- "$stage"
  trap - EXIT
}

main "$@"
