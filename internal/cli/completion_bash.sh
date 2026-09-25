# Data Mate's Bash 3.2 adapter uses only shell builtins and literal argv.
# Keep completion state local to Cobra's caller; do not change caller options.
__data-mate_literal_word() {
  local input="$1" character mode=plain escaped=false i
  REPLY=
  for ((i = 0; i < ${#input}; i++)); do
    character="${input:i:1}"
    if [[ "$escaped" == true ]]; then
      REPLY+="$character"
      escaped=false
      continue
    fi
    case "$mode:$character" in
    "plain:'") mode=single ;;
    'plain:"') mode=double ;;
    'plain:\' | 'double:\') escaped=true ;;
    "single:'" | 'double:"') mode=plain ;;
    *) REPLY+="$character" ;;
    esac
  done
  if [[ "$escaped" == true ]]; then REPLY+='\'; fi
}
__data-mate_init_completion() {
  COMPREPLY=()
  words=()
  local i value REPLY join=false
  cword=0
  for ((i = 0; i <= COMP_CWORD; i++)); do
    __data-mate_literal_word "${COMP_WORDS[i]}"
    value="$REPLY"
    if [[ "$value" == = || "$value" == : ]]; then
      if ((${#words[@]})); then
        words[${#words[@]} - 1]+="$value"
        join=true
        continue
      fi
    fi
    if [[ "$join" == true ]]; then
      words[${#words[@]} - 1]+="$value"
      join=false
    else
      words[${#words[@]}]="$value"
    fi
  done
  cword=$((${#words[@]} - 1))
  cur="${words[cword]}"
  prev=
  if ((cword > 0)); then prev="${words[cword - 1]}"; fi
}
__data-mate_get_completion_results() {
  local args=("${words[@]:1}")
  out=$("${words[0]}" __complete "${args[@]}" 2>/dev/null)
  directive="${out##*$'\n':}"
  if [[ "$out" == :* && "$out" != *$'\n'* ]]; then
    directive="${out#:}"
    out=
  else out="${out%$'\n':*}"; fi
  [[ "$directive" =~ ^[0-9]+$ ]] || {
    directive=1
    out=
  }
  if [[ "$cur" == -*=* ]]; then cur="${cur#*=}"; fi
}
__data-mate_process_completion_results() {
  COMPREPLY=()
  ((directive & 1)) && return
  local candidate quoted filter extension found
  if ((directive & 8 || directive & 16 || directive == 0)); then
    while IFS= read -r candidate; do
      if ((directive & 16)) && [[ ! -d "$candidate" ]]; then continue; fi
      if ((directive & 8)) && [[ ! -d "$candidate" ]]; then
        found=false
        while IFS= read -r filter; do
          extension="${filter%%$'\t'*}"
          [[ "$candidate" == *."$extension" ]] && found=true
        done <<<"$out"
        [[ "$found" == true ]] || continue
      fi
      if [[ -d "$candidate" ]]; then candidate+=/; fi
      printf -v quoted '%q' "$candidate"
      COMPREPLY[${#COMPREPLY[@]}]="$quoted"
    done < <(compgen -f -- "$cur")
    return
  fi
  while IFS= read -r candidate; do
    candidate="${candidate%%$'\t'*}"
    [[ -n "$candidate" && "$candidate" == "$cur"* ]] || continue
    printf -v quoted '%q' "$candidate"
    COMPREPLY[${#COMPREPLY[@]}]="$quoted"
  done <<<"$out"
}
# No arbitrary filename fallback, including on system Bash without compopt.
complete -F __start_data-mate data-mate
