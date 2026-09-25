#!/bin/bash
# Uses the disposable hosted runner's real account home. Never run on a workstation.
set -euo pipefail
export PATH=/usr/bin:/bin:/usr/sbin:/sbin
[[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted ]] || {
  echo 'Release acceptance requires a disposable GitHub-hosted runner.' >&2
  exit 1
}
[[ "$EUID" != 0 && "$(uname -m)" == arm64 && "$(sw_vers -productVersion)" == 15.* ]]
[[ "$#" == 2 && "$1" == /* && "$2" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
candidate_dir="$1"
tag="$2"
candidate="$candidate_dir/data-mate_darwin_arm64"
account_record="$(dscl . -read "/Users/$(id -un)" NFSHomeDirectory)"
account_home="${account_record#NFSHomeDirectory: }"
[[ "$account_home" == /* && "$account_home" != "$account_record" ]]
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

for name in install.sh data-mate_darwin_arm64 release.txt SHA256SUMS; do
  [[ -f "$candidate_dir/$name" && ! -L "$candidate_dir/$name" ]]
done
[[ "$(stat -f '%z' "$candidate_dir/SHA256SUMS")" -le 65536 && "$(stat -f '%z' "$candidate_dir/release.txt")" -le 4096 ]]
awk 'NF != 2 || length($1) != 64 || $1 ~ /[^0-9a-f]/ || $2 !~ /^(install.sh|data-mate_darwin_arm64|release.txt)$/ || seen[$2]++ {bad=1} END {exit (bad || NR != 3)}' "$candidate_dir/SHA256SUMS"
(cd "$candidate_dir" && shasum -a 256 -c SHA256SUMS)
case "$(file -b "$candidate")" in 'Mach-O 64-bit executable arm64'*) ;; *) exit 1 ;; esac
codesign --verify --strict --check-notarization -R '=anchor apple generic and identifier "io.github.swqa7697.data-mate" and certificate leaf[subject.OU] = "JDNRPL924Q" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and notarized' "$candidate"
chmod 700 "$candidate"
"$candidate" __release-metadata --text >"$scratch/compiled.txt"
cmp "$candidate_dir/release.txt" "$scratch/compiled.txt"
"$candidate" __release-metadata >"$scratch/metadata.json"
[[ "$(plutil -extract version raw -o - "$scratch/metadata.json")" == "${tag#v}" ]]
[[ "$(plutil -extract platform raw -o - "$scratch/metadata.json")" == darwin_arm64 ]]

# No Go setup, Homebrew, or build commands. Hosted images still contain developer
# tools; restricting PATH proves this smoke path uses only the candidate/system tools.
cd "$scratch"
installed_once=true
"$candidate" __install
[[ -x "$installed" && -L "$command_link" && "$(readlink "$command_link")" == "$installed" ]]
"$command_link" version
"$installed" db list --json >connections.json
[[ "$(plutil -extract connections json -o - connections.json)" == '[]' ]]
"$installed" mcp status --json >status.json
[[ "$(plutil -extract state raw -o - status.json)" == stopped ]]
[[ "$(plutil -extract keyset_state raw -o - status.json)" == absent ]]
[[ "$(plutil -extract mcp_enabled raw -o - status.json)" == false ]]
"$installed" completion bash >completion.bash
"$installed" completion zsh >completion.zsh
/bin/bash --noprofile --norc -c 'source "$1"; export PATH="$2:$PATH"; COMP_WORDS=(data-mate d); COMP_CWORD=1; COMP_LINE="data-mate d"; COMP_POINT=11; __start_data-mate; [[ " ${COMPREPLY[*]} " == *" db "* ]]' completion "$scratch/completion.bash" "$account_home/.local/bin"
/bin/zsh -f -c 'autoload -Uz compinit; compinit -D; source "$1"; (( $+functions[_data-mate] )); result=$("$2" __complete d); [[ "$result" == *db* ]]' completion "$scratch/completion.zsh" "$installed"
installation_id="$(plutil -extract installation raw -o - "$root/distribution.json")"
[[ -n "$installation_id" ]]
"$candidate" __install
"$installed" uninstall --yes
[[ ! -e "$installed" && ! -L "$command_link" && -f "$root/data-mate.db" ]]
[[ "$(plutil -extract installation raw -o - "$root/distribution.json")" == "$installation_id" ]]
"$candidate" db list --json >connections-retained.json
cmp connections.json connections-retained.json
"$candidate" __install
[[ "$(plutil -extract installation raw -o - "$root/distribution.json")" == "$installation_id" ]]
"$installed" db list --json >connections-after.json
cmp connections.json connections-after.json
"$installed" uninstall --purge --yes
[[ ! -e "$root" && ! -L "$command_link" ]]
installed_once=false
echo 'Signed candidate acceptance passed on fresh macOS 15 arm64.'
