#!/bin/bash
# Crawl the free Gazette on pcm.gov.lb a little every day, until everything in
# the phases file is on disk.
#
# PCM answers with a CAPTCHA after a few hundred fetches (79 at 1.5 s apart on
# 13 Oct 2026, 570 at about 12 s apart on 16 Oct). So each run takes at most a
# daily budget of fetches, slowly, and stops for the day at the first challenge.
# A challenge sets the next budget to 70% of what got through; a clean day
# raises it by 25, up to PCM_MAX. Nothing is retried after a challenge, and the
# CAPTCHA is never solved: the next run clears the stop file only on a later day.
#
# Usage: pcm-daily.sh DATA_DIR [PHASES_FILE]
# Env:   PCM_GAP (15s), PCM_START (400), PCM_MAX (450), PCM_MIN (50),
#        PCM_TZ (Asia/Beirut), PCM_NOTIFY (shell command, gets a summary on stdin),
#        LEBGAZETTE (path to the binary), FORCE=1 (run again on the same day).
set -u
here=$(cd "$(dirname "$0")" && pwd)
G=${LEBGAZETTE:-$here/../lebgazette}
D=${1:?usage: pcm-daily.sh DATA_DIR [PHASES_FILE]}
P=${2:-$here/pcm-phases.txt}
GAP=${PCM_GAP:-15s}
MAX=${PCM_MAX:-450}
MIN=${PCM_MIN:-50}
TZL=${PCM_TZ:-Asia/Beirut}
cd "$D" || exit 1

today=$(TZ=$TZL date +%F)
budget=$(cat pcm-budget 2>/dev/null || echo "${PCM_START:-400}")
used=0

log() { echo "$(TZ=$TZL date '+%F %T') $*" >> pcm-daily.log; echo "$*"; }
notify() { [ -n "${PCM_NOTIFY:-}" ] && echo "$*" | sh -c "$PCM_NOTIFY"; return 0; }

if [ "$(cat pcm-last-run 2>/dev/null)" = "$today" ] && [ "${FORCE:-}" != 1 ]; then
  log "already ran today"; exit 0
fi
if [ -s BLOCKED ]; then
  when=$(head -1 BLOCKED)
  bday=$(TZ=$TZL date -d "$when" +%F 2>/dev/null || echo "$today")
  if [ "$bday" = "$today" ]; then log "challenged today ($when), waiting for tomorrow"; exit 0; fi
  { echo "== cleared $(date -u +%FT%TZ)"; cat BLOCKED; } >> pcm-blocks.log
  rm -f BLOCKED
  log "cleared the stop file from $bday"
fi
echo "$today" > pcm-last-run

# run CMD ARGS...: one lebgazette run; adds its network fetches to $used.
run() {
  local out rc n
  out=$("$G" "$@" -data . -gap "$GAP" </dev/null 2>&1); rc=$?
  n=$(printf '%s\n' "$out" | sed -n 's/^network fetches: //p' | tail -1)
  used=$((used + ${n:-0}))
  printf '%s\n' "$out" | grep -E '^(error|warn|year )' | tail -5 >> pcm-daily.log
  last=$(printf '%s\n' "$out" | grep '^error:' | tail -1)
  return $rc
}

finish() { # $1 = outcome
  case $1 in
    challenged) budget=$(( used * 7 / 10 )); [ $budget -lt $MIN ] && budget=$MIN ;;
    spent)      budget=$(( budget + 25 )); [ $budget -gt $MAX ] && budget=$MAX ;;
  esac
  echo $budget > pcm-budget
  "$G" index -data . >/dev/null 2>&1
  msg="pcm-daily $today: $1 after $used fetches, next budget $budget"
  log "$msg"
  case $1 in challenged|error) notify "$msg${last:+ ($last)}" ;; esac
  [ "$1" = error ] && exit 1
  exit 0
}

log "start, budget $budget, gap $GAP"
run discover -refresh || { [ -s BLOCKED ] && finish challenged; finish error; }

while read -r years kinds extra; do
  case $years in ''|'#'*) continue ;; esac
  left=$(( budget - used ))
  [ $left -le 0 ] && finish spent
  k=(); [ "$kinds" != - ] && k=(-kinds "$kinds")
  # shellcheck disable=SC2086 # extra holds flags like -part2
  if run images -years "$years" "${k[@]}" -limit "$left" $extra; then
    log "phase done: $years $kinds $extra"
    continue
  fi
  [ -s BLOCKED ] && finish challenged
  [[ $last == *"limit reached"* ]] && finish spent
  finish error
done < "$P"
finish complete
