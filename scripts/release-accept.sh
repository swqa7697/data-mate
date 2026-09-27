#!/bin/bash
# Uses the disposable hosted runner's real account home. Never run on a workstation.
set -euo pipefail
export PATH=/usr/bin:/bin:/usr/sbin:/sbin
[[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted ]] || {
  echo 'Release acceptance requires a disposable GitHub-hosted runner.' >&2
  exit 1
}
[[ "$EUID" != 0 ]]
source "$(dirname "$0")/install-release.sh"
detect_platform
if [[ "$platform" == darwin_arm64 ]]; then [[ "$(sw_vers -productVersion)" == 15.* ]]; fi
json_value() {
  if [[ "$platform" == darwin_arm64 ]]; then /usr/bin/plutil -extract "$1" "$2" -o - "$3"; else
    /usr/bin/python3 -c 'import json,sys; v=json.load(open(sys.argv[2]))[sys.argv[1]]; print(json.dumps(v,separators=(",",":")) if not isinstance(v,str) else v)' "$1" "$3"
  fi
}
[[ "$#" == 2 && "$1" == /* && "$2" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
candidate_dir="$1"
tag="$2"
candidate="$candidate_dir/$binary_name"
if [[ "$platform" == darwin_arm64 ]]; then
  account_record="$(dscl . -read "/Users/$(id -un)" NFSHomeDirectory)"
  account_home="${account_record#NFSHomeDirectory: }"
  [[ "$account_home" != "$account_record" ]]
else
  account_home="$(getent passwd "$(id -u)" | cut -d: -f6)"
fi
[[ "$account_home" == /* ]]
root="$account_home/.local/share/data-mate"
installed="$root/bin/data-mate"
command_link="$account_home/.local/bin/data-mate"
[[ ! -e "$root" && ! -L "$root" && ! -e "$command_link" && ! -L "$command_link" ]] || {
  echo 'Acceptance refuses an existing production installation.' >&2
  exit 1
}
unset ZDOTDIR
umask 077
scratch="$(mktemp -d /tmp/data-mate-accept.XXXXXX)"
installed_once=false
cleanup() {
  if [[ "$installed_once" == true && -d "$root" ]]; then
    "$candidate" __uninstall --purge || echo 'Acceptance cleanup failed; inspect this disposable runner.' >&2
  fi
  rm -rf "$scratch"
}
trap cleanup EXIT

stage="$candidate_dir"
for name in install.sh "$binary_name" "$metadata_name" "$sums_name"; do
  [[ -f "$candidate_dir/$name" && ! -L "$candidate_dir/$name" ]]
done
[[ "$(file_size "$candidate_dir/$sums_name")" -le 65536 && "$(file_size "$candidate_dir/$metadata_name")" -le 4096 ]]
# Reuse the bounded bootstrap parser, preserving the existing acceptance scope.
version="${tag#v}"
metadata
verify_checksums true
verify_signature "$candidate"
if [[ "$platform" == darwin_arm64 ]]; then
  case "$(file -b "$candidate")" in 'Mach-O 64-bit executable arm64'*) ;; *) exit 1 ;; esac
  codesign --verify --strict --check-notarization -R '=anchor apple generic and identifier "io.github.swqa7697.data-mate" and certificate leaf[subject.OU] = "JDNRPL924Q" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and notarized' "$candidate"
else
  case "$(file -b "$candidate")" in 'ELF 64-bit LSB '*x86-64*) ;; *) exit 1 ;; esac
fi
chmod 700 "$candidate"
"$candidate" __release-metadata --text >"$scratch/compiled.txt"
cmp "$candidate_dir/$metadata_name" "$scratch/compiled.txt"
"$candidate" __release-metadata >"$scratch/metadata.json"
[[ "$(json_value version raw "$scratch/metadata.json")" == "${tag#v}" ]]
[[ "$(json_value platform raw "$scratch/metadata.json")" == "$platform" ]]

# No Go setup, Homebrew, or build commands. Hosted images still contain developer
# tools; restricting PATH proves this smoke path uses only the candidate/system tools.
cd "$scratch"
installed_once=true
"$candidate" __install
[[ -x "$installed" && -L "$command_link" && "$(readlink "$command_link")" == "$installed" ]]
"$command_link" version
"$installed" db list --json >connections.json
[[ "$(json_value connections json connections.json)" == '[]' ]]
"$installed" mcp status --json >status.json
[[ "$(json_value state raw status.json)" == stopped ]]
[[ "$(json_value keyset_state raw status.json)" == absent ]]
[[ "$(json_value mcp_enabled raw status.json)" == false ]]
"$installed" completion bash >completion.bash
"$installed" completion zsh >completion.zsh
/bin/bash --noprofile --norc -c 'source "$1"; export PATH="$2:$PATH"; COMP_WORDS=(data-mate d); COMP_CWORD=1; COMP_LINE="data-mate d"; COMP_POINT=11; __start_data-mate; [[ " ${COMPREPLY[*]} " == *" db "* ]]' completion "$scratch/completion.bash" "$account_home/.local/bin"
if [[ "$platform" == darwin_arm64 || -x /bin/zsh ]]; then
  /bin/zsh -f -c 'autoload -Uz compinit; compinit -D; source "$1"; (( $+functions[_data-mate] )); result=$("$2" __complete d); [[ "$result" == *db* ]]' completion "$scratch/completion.zsh" "$installed"
fi
installation_id="$(json_value installation raw "$root/distribution.json")"
[[ -n "$installation_id" ]]
"$candidate" __install
"$installed" uninstall --yes
[[ ! -e "$installed" && ! -L "$command_link" && -f "$root/data-mate.db" ]]
[[ "$(json_value installation raw "$root/distribution.json")" == "$installation_id" ]]
"$candidate" db list --json >connections-retained.json
cmp connections.json connections-retained.json
"$candidate" __install
[[ "$(json_value installation raw "$root/distribution.json")" == "$installation_id" ]]
"$installed" db list --json >connections-after.json
cmp connections.json connections-after.json
"$installed" uninstall --purge --yes
[[ ! -e "$root" && ! -L "$command_link" ]]
installed_once=false
printf 'Signed candidate acceptance passed on %s.\n' "$platform"
