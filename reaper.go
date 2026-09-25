package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The reaper is an external /bin/sh child holding the write end of a
// pipe open. Whatever way this process dies (SIGKILL included), the
// pipe reaches EOF, and the reaper force-deletes every registered
// container. While this process lives, the reaper does nothing;
// deletion is the job of Terminate/Cleanup, the reaper is insurance.
//
// The script is a fixed string; container IDs enter it only as stdin
// data validated as an Apple Container name or a full Docker ID, and
// the script itself disables globbing and quotes every expansion the
// IDs reach.
// Every backend entry runs behind a bounded timeout. The timeout covers
// the complete inspect, status-marker, filter, and delete pipeline; it
// snapshots and validates descendants before signaling them, while helper
// process groups are isolated and reaped. Inspect output is streamed
// through a field filter instead of being staged on disk. Failures stay
// silent (|| true) by design: the reaper is last-resort insurance.
// When a creation generation is known, the script inspects first and
// reads the creation label as a structural JSON field: the match is
// anchored at line start on the quoted key, so label values or other
// text containing the same characters cannot satisfy it. Inspect output
// is filtered before command substitution, so only the generation, ID,
// and status are held in memory; it is never staged in a host file, so
// a killed reaper cannot leave an environment-bearing inspect dump behind.
// When inspect also reports an immutable "Id" (Docker), the delete
// targets that validated ID instead of the name; a Docker entry with a
// generation but no valid ID is skipped.
// Apple Container has no such ID; there the delete necessarily goes by
// name.
const reaperScript = `set -f
bin="$1"
sub="$2"
key="$3"
timeout="${4:-30}"
max_registered_entries=1024
registered_entries=0
ids=""
while IFS= read -r line; do
  [ "$registered_entries" -lt "$max_registered_entries" ] || break
  registered_entries=$((registered_entries + 1))
  [ "${#line}" -le 256 ] 2>/dev/null || continue
  ids="$ids
$line"
done
descendant_timeout=1
helper_timeout=2
max_descendant_pids=256
max_descendant_depth=32
max_descendant_lookups=64
helper_output_blocks=512
cleanup_helper_budget=0
cleanup_active=0
work_dir="$5"
awk_bin="$6"
pgrep_bin="$7"
ps_bin="$8"
rm_bin="$9"
sleep_bin="${10:-}"
ps_start_field="${11:-lstart}"
case "$ps_start_field" in
  lstart|stime) ;;
  *) exit 0 ;;
esac
case "$timeout" in
  ''|*[!0-9]*) timeout=30 ;;
esac
[ "$timeout" -gt 0 ] 2>/dev/null || timeout=30
case "$work_dir" in
  /*) [ -d "$work_dir" ] || exit 0 ;;
  *) exit 0 ;;
esac
for helper_path in "$awk_bin" "$ps_bin" "$rm_bin" "$sleep_bin"; do
  case "$helper_path" in
    /*) [ -x "$helper_path" ] || exit 0 ;;
    *) exit 0 ;;
  esac
done
case "$pgrep_bin" in
  "") pgrep_disabled=1 ;;
  /*) [ -x "$pgrep_bin" ] || exit 0 ;;
  *) exit 0 ;;
esac
ulimit -f "$helper_output_blocks" 2>/dev/null || exit 0
reaper_warn() {
  printf 'containergo: %s\n' "$*" >&2
}
monitor_enabled() {
  if [ "${CONTAINERGO_REAPER_DISABLE_MONITOR:-}" = 1 ]; then
    return 1
  fi
  monitor_state=$(set -o 2>/dev/null) || monitor_state=
  case "$monitor_state" in
    *"monitor on"*) return 0 ;;
  esac
  return 1
}
helper_tree_pids=
helper_tree_count=0
helper_tree_known=
helper_tree_table_result=0
build_helper_tree() {
  helper_tree_root="$1"
  helper_tree_file="$2"
  helper_tree_known=" $helper_tree_root"
  helper_tree_pids=
  helper_tree_edges=
  helper_tree_count=1
  helper_tree_added=1
  while [ "$helper_tree_added" -eq 1 ]; do
    helper_tree_added=0
    while IFS=' ' read -r tree_pid tree_ppid tree_rest; do
      [ -n "$tree_pid" ] && [ -n "$tree_ppid" ] && [ -n "$tree_rest" ] || { helper_tree_table_result=1; break; }
      case "$tree_pid" in *[!0-9]*) helper_tree_table_result=1; break ;; esac
      case "$tree_ppid" in *[!0-9]*) helper_tree_table_result=1; break ;; esac
      case " $helper_tree_known " in
        *" $tree_pid "*) continue ;;
      esac
      case " $helper_tree_known " in
        *" $tree_ppid "*)
          printf '%s\n' "$tree_rest" >"$work_dir/identity.$tree_pid" 2>/dev/null || { helper_tree_table_result=1; break; }
          helper_tree_known="$helper_tree_known $tree_pid"
          helper_tree_pids="$helper_tree_pids $tree_pid"
          helper_tree_edges="$helper_tree_edges $tree_ppid:$tree_pid"
          helper_tree_count=$((helper_tree_count + 1))
          if [ "$helper_tree_count" -gt "$max_descendant_pids" ]; then
            helper_tree_table_result=1
            break
          fi
          helper_tree_added=1
          ;;
      esac
    done <"$helper_tree_file"
    [ "$helper_tree_table_result" -eq 0 ] || break
  done
  helper_tree_pids="${helper_tree_pids# }"
  return "$helper_tree_table_result"
}
kill_helper_descendants() {
  helper_tree_root="$1"
  helper_tree_table="$work_dir/helper.table"
  helper_tree_error="$work_dir/helper.err"
  saved_helper_pid="$helper_pid"
  saved_helper_timer="$helper_timer"
  saved_helper_groups="$helper_process_groups"
  saved_helper_enumeration="$helper_enumeration"
  helper_enumeration=1
  run_helper "$helper_tree_table" "$helper_tree_error" "$ps_bin" -e -o pid= -o ppid= -o "$ps_start_field="
  helper_table_status="$?"
  helper_pid="$saved_helper_pid"
  helper_timer="$saved_helper_timer"
  helper_process_groups="$saved_helper_groups"
  helper_enumeration="$saved_helper_enumeration"
  helper_tree_table_result=0
  if [ "$helper_table_status" -eq 0 ]; then
    build_helper_tree "$helper_tree_root" "$helper_tree_table" || true
    for tree_pid in $helper_tree_pids; do
      kill -0 "$tree_pid" 2>/dev/null || continue
      kill -KILL "$tree_pid" 2>/dev/null || true
    done
  fi
}
run_helper() {
  helper_out="$1"
  helper_err="$2"
  shift 2
  if [ "$cleanup_active" = 1 ]; then
    [ "$cleanup_helper_budget" -gt 0 ] || return 125
    cleanup_helper_budget=$((cleanup_helper_budget - 1))
  fi
  : >"$helper_out" 2>/dev/null || return 125
  : >"$helper_err" 2>/dev/null || return 125
  set -m 2>/dev/null
  helper_process_groups=0
  if monitor_enabled; then
    helper_process_groups=1
  fi
  "$@" >"$helper_out" 2>"$helper_err" &
  helper_pid="$!"
  (
    helper_sleeper=
    cleanup_helper_timer() {
      if [ -n "$helper_sleeper" ]; then
        kill -KILL "$helper_sleeper" 2>/dev/null || true
        wait "$helper_sleeper" 2>/dev/null || true
      fi
    }
    trap 'cleanup_helper_timer; exit 0' HUP INT TERM
    "$sleep_bin" "$helper_timeout" &
    helper_sleeper="$!"
    wait "$helper_sleeper"
    helper_sleeper=
    if [ "$helper_process_groups" = 1 ]; then
      kill -KILL -"$helper_pid" 2>/dev/null || true
    elif [ "$helper_enumeration" != 1 ]; then
      kill_helper_descendants "$helper_pid"
    fi
    kill -KILL "$helper_pid" 2>/dev/null || true
    wait "$helper_pid" 2>/dev/null || true
  ) &
  helper_timer="$!"
  wait "$helper_pid"
  helper_status="$?"
  kill -TERM "$helper_timer" 2>/dev/null || true
  wait "$helper_timer" 2>/dev/null || true
  set +m
  return "$helper_status"
}
remove_files() {
  # The parent removes the private work directory after Wait. Defer the
  # pinned rm helper to one bounded batch after all entries are processed.
  :
}
capture_process_table() {
  process_table="$work_dir/process.table"
  process_table_error="$work_dir/process-table.err"
  saved_process_helper_enumeration="$helper_enumeration"
  helper_enumeration=1
  run_helper "$process_table" "$process_table_error" "$ps_bin" -e -o pid= -o ppid= -o "$ps_start_field="
  process_table_status="$?"
  helper_enumeration="$saved_process_helper_enumeration"
  if [ "$process_table_status" -ne 0 ]; then
    reaper_warn "process table lookup failed (status $process_table_status)"
    return 1
  fi
  process_table_ready=1
  return 0
}
list_children_from_table() {
  table_parent="$1"
  table_children=
  table_count=0
  table_result=0
  while IFS=' ' read -r table_pid table_ppid table_rest; do
    [ "$table_ppid" = "$table_parent" ] || continue
    case "$table_pid" in
      ""|*[!0-9]*) table_result=1; break ;;
    esac
    [ "$table_pid" -gt 0 ] 2>/dev/null || { table_result=1; break; }
    [ -n "$table_rest" ] || { table_result=1; break; }
    printf '%s\n' "$table_rest" >"$work_dir/identity.$table_pid" 2>/dev/null || { table_result=1; break; }
    table_count=$((table_count + 1))
    if [ "$table_count" -gt "$max_descendant_pids" ]; then
      table_result=1
      break
    fi
    table_children="$table_children $table_pid"
  done <"$process_table"
  if [ "$table_result" -ne 0 ]; then
    reaper_warn "process table returned invalid descendants for pid $table_parent"
    return 1
  fi
  table_children="${table_children# }"
  return 0
}
capture_identity() {
  identity_pid="$1"
  identity_file="$2"
  identity_raw="$identity_file.raw"
  identity_error="$identity_file.err"
  saved_identity_helper_enumeration="$helper_enumeration"
  helper_enumeration=1
  run_helper "$identity_raw" "$identity_error" "$ps_bin" -o pid= -o "$ps_start_field=" -p "$identity_pid"
  identity_status="$?"
  helper_enumeration="$saved_identity_helper_enumeration"
  if [ "$identity_status" -eq 1 ] && [ ! -s "$identity_error" ]; then
    remove_files "$identity_raw" "$identity_error" || true
    return 1
  fi
  if [ "$identity_status" -ne 0 ]; then
    reaper_warn "process identity lookup failed for pid $identity_pid (status $identity_status)"
    remove_files "$identity_raw" "$identity_error" || true
    return 125
  fi
  identity_value=
  identity_lines=0
  while IFS= read -r identity_line || [ -n "$identity_line" ]; do
    identity_lines=$((identity_lines + 1))
    case "$identity_line" in
      "$identity_pid "*)
        identity_value=${identity_line#"$identity_pid"}
        ;;
      *)
        reaper_warn "process identity output was malformed for pid $identity_pid"
        remove_files "$identity_raw" "$identity_error" || true
        return 125
        ;;
    esac
  done <"$identity_raw"
  remove_files "$identity_raw" "$identity_error" || true
  if [ "$identity_lines" -ne 1 ] || [ -z "$identity_value" ] || [ "${#identity_value}" -gt 256 ] 2>/dev/null; then
    reaper_warn "process identity output was invalid for pid $identity_pid"
    return 125
  fi
  printf '%s\n' "$identity_value" >"$identity_file" 2>/dev/null || return 125
  return 0
}
revalidate_identity() {
  revalidate_pid="$1"
  revalidate_stored="$work_dir/identity.$revalidate_pid"
  revalidate_current="$revalidate_stored.current"
  capture_identity "$revalidate_pid" "$revalidate_current"
  revalidate_status="$?"
  if [ "$revalidate_status" -eq 1 ]; then
    return 2
  fi
  [ "$revalidate_status" -eq 0 ] || return "$revalidate_status"
  revalidate_old=
  revalidate_new=
  while IFS= read -r revalidate_line || [ -n "$revalidate_line" ]; do
    [ -n "$revalidate_line" ] && revalidate_old=$revalidate_line
    break
  done <"$revalidate_stored"
  while IFS= read -r revalidate_line || [ -n "$revalidate_line" ]; do
    [ -n "$revalidate_line" ] && revalidate_new=$revalidate_line
    break
  done <"$revalidate_current"
  remove_files "$revalidate_current" || true
  if [ -z "$revalidate_old" ] || [ -z "$revalidate_new" ]; then
    return 125
  fi
  [ "$revalidate_old" = "$revalidate_new" ] || return 1
  return 0
}
is_tombstoned() {
  case " $tombstoned_pids " in
    *" $1 "*) return 0 ;;
  esac
  return 1
}
mark_tombstoned() {
  is_tombstoned "$1" || tombstoned_pids="$tombstoned_pids $1"
}
is_stopped() {
  case " $stopped_pids " in
    *" $1 "*) return 0 ;;
  esac
  return 1
}
mark_stopped() {
  is_stopped "$1" || stopped_pids="$stopped_pids $1"
}
list_children() {
  parent="$1"
  descendant_lookups=$((descendant_lookups + 1))
  if [ "$descendant_lookups" -gt "$max_descendant_lookups" ]; then
    reaper_warn "descendant lookup limit reached"
    return 1
  fi
  pgrep_out="$work_dir/pgrep.out"
  pgrep_err="$work_dir/pgrep.err"
  pgrep_children=
  pgrep_valid=0
  if [ "$pgrep_disabled" = 1 ]; then
    pgrep_status=125
  else
    run_helper "$pgrep_out" "$pgrep_err" "$pgrep_bin" -P "$parent"
    pgrep_status="$?"
    if [ "$pgrep_status" -eq 1 ] && [ -s "$pgrep_err" ]; then
      pgrep_status=125
    fi
    case "$pgrep_status" in
      0|1)
        pgrep_valid=1
        pgrep_count=0
        while IFS= read -r child || [ -n "$child" ]; do
          case "$child" in
            ""|*[!0-9]*) pgrep_valid=0; break ;;
          esac
          [ "$child" -gt 0 ] 2>/dev/null || { pgrep_valid=0; break; }
          pgrep_count=$((pgrep_count + 1))
          if [ "$pgrep_count" -gt "$max_descendant_pids" ]; then
            pgrep_valid=0
            break
          fi
          pgrep_children="$pgrep_children $child"
        done <"$pgrep_out"
        ;;
      *)
        reaper_warn "pgrep lookup failed for pid $parent (status $pgrep_status)"
        ;;
    esac
  fi
  pgrep_children="${pgrep_children# }"
  if [ "$pgrep_valid" -ne 1 ]; then
    pgrep_children=
    pgrep_disabled=1
  fi
  table_valid=0
  table_children=
  if [ "$process_table_ready" = 1 ]; then
    list_children_from_table "$parent"
    table_status="$?"
    if [ "$table_status" -eq 0 ]; then
      table_valid=1
    else
      reaper_warn "process table lookup failed for pid $parent"
    fi
  fi
  if [ "$pgrep_valid" -eq 0 ] && [ "$table_valid" -eq 0 ]; then
    reaper_warn "no validated descendant snapshot for pid $parent"
    return 1
  fi
  children=
  for child in $pgrep_children; do
    case " $children " in
      *" $child "*) ;;
      *) children="$children $child" ;;
    esac
  done
  for child in $table_children; do
    case " $children " in
      *" $child "*) ;;
      *) children="$children $child" ;;
    esac
  done
  children="${children# }"
  return 0
}
snapshot_branch() {
  snapshot_parent="$1"
  snapshot_depth="$2"
  if [ "$snapshot_depth" -gt "$max_descendant_depth" ]; then
    reaper_warn "descendant depth limit reached at pid $snapshot_parent"
    snapshot_error=1
    return 1
  fi
  if is_tombstoned "$snapshot_parent"; then
    return 0
  fi
  # The quiesced ps table carries a start-time identity for the current tree.
  # A PID is marked stopped only after SIGSTOP succeeds; it cannot be recycled
  # before the direct signal without an intervening exit.
  if [ "$process_table_ready" = 1 ] && [ -s "$work_dir/identity.$snapshot_parent" ]; then
    snapshot_status=0
  else
    revalidate_identity "$snapshot_parent"
    snapshot_status="$?"
  fi
  if [ "$snapshot_status" -eq 2 ]; then
    mark_tombstoned "$snapshot_parent"
    return 0
  fi
  if [ "$snapshot_status" -ne 0 ]; then
    mark_tombstoned "$snapshot_parent"
    snapshot_error=1
    return 1
  fi
  case " $pass_seen " in
    *" $snapshot_parent "*) return 0 ;;
  esac
  pass_seen="$pass_seen $snapshot_parent"
  case " $known_parents " in
    *" $snapshot_parent "*) ;;
    *)
      known_parents="$known_parents $snapshot_parent"
      known_count=$((known_count + 1))
      if [ "$known_count" -gt "$max_descendant_pids" ]; then
        reaper_warn "descendant pid limit reached"
        snapshot_error=1
        return 1
      fi
      ;;
  esac
  list_children "$snapshot_parent"
  snapshot_status="$?"
  if [ "$snapshot_status" -ne 0 ]; then
    snapshot_error=1
    return 1
  fi
  # The quiesced ps table carries a start-time identity for the current tree.
  # A PID is marked stopped only after SIGSTOP succeeds; it cannot be recycled
  # before the direct signal without an intervening exit.
  if [ "$process_table_ready" = 1 ] && [ -s "$work_dir/identity.$snapshot_parent" ]; then
    snapshot_status=0
  else
    revalidate_identity "$snapshot_parent"
    snapshot_status="$?"
  fi
  if [ "$snapshot_status" -eq 2 ]; then
    mark_tombstoned "$snapshot_parent"
    return 0
  fi
  if [ "$snapshot_status" -ne 0 ]; then
    mark_tombstoned "$snapshot_parent"
    snapshot_error=1
    return 1
  fi
  snapshot_children="$children"
  for snapshot_child in $snapshot_children; do
    if is_tombstoned "$snapshot_child"; then
      continue
    fi
    if [ "$process_table_ready" = 1 ] && [ -s "$work_dir/identity.$snapshot_child" ]; then
      snapshot_status=0
    else
      capture_identity "$snapshot_child" "$work_dir/identity.$snapshot_child"
      snapshot_status="$?"
      if [ "$snapshot_status" -eq 1 ]; then
        mark_tombstoned "$snapshot_child"
        continue
      fi
      if [ "$snapshot_status" -ne 0 ]; then
        snapshot_error=1
        break
      fi
      revalidate_identity "$snapshot_child"
      snapshot_status="$?"
      if [ "$snapshot_status" -eq 2 ]; then
        mark_tombstoned "$snapshot_child"
        continue
      fi
      if [ "$snapshot_status" -ne 0 ]; then
        mark_tombstoned "$snapshot_child"
        snapshot_error=1
        break
      fi
    fi
    if kill -STOP "$snapshot_child" 2>/dev/null; then
      mark_stopped "$snapshot_child"
    fi
    snapshot_branch "$snapshot_child" "$((snapshot_depth + 1))" || snapshot_error=1
  done
  snapshot="$snapshot $1"
  return 0
}
signal_snapshot() {
  for snapshot_pid in $snapshot; do
    if is_tombstoned "$snapshot_pid"; then
      continue
    fi
    if is_stopped "$snapshot_pid"; then
      kill -9 "$snapshot_pid" 2>/dev/null || true
      continue
    fi
    revalidate_identity "$snapshot_pid"
    snapshot_status="$?"
    if [ "$snapshot_status" -eq 2 ]; then
      mark_tombstoned "$snapshot_pid"
      continue
    fi
    if [ "$snapshot_status" -ne 0 ]; then
      mark_tombstoned "$snapshot_pid"
      reaper_warn "process identity changed before killing pid $snapshot_pid"
      continue
    fi
    kill -9 "$snapshot_pid" 2>/dev/null || true
  done
}
direct_snapshot_fallback() {
  for snapshot_pid in $snapshot; do
    if is_tombstoned "$snapshot_pid"; then
      continue
    fi
    if is_stopped "$snapshot_pid"; then
      kill -9 "$snapshot_pid" 2>/dev/null || true
      continue
    fi
    revalidate_identity "$snapshot_pid"
    snapshot_status="$?"
    if [ "$snapshot_status" -eq 0 ]; then
      kill -9 "$snapshot_pid" 2>/dev/null || true
    elif [ "$snapshot_status" -eq 2 ]; then
      mark_tombstoned "$snapshot_pid"
    fi
  done
  kill -9 "$kill_root_pid" 2>/dev/null || true
}
same_pid_list() {
  same_left_count=0
  for same_pid in $1; do
    same_left_count=$((same_left_count + 1))
  done
  same_right_count=0
  for same_pid in $2; do
    same_right_count=$((same_right_count + 1))
  done
  [ "$same_left_count" -eq "$same_right_count" ] || return 1
  for same_pid in $1; do
    case " $2 " in
      *" $same_pid "*) ;;
      *) return 1 ;;
    esac
  done
  return 0
}
capture_quiesced_process_table() {
  quiesce_root="$1"
  quiesce_previous=
  quiesce_previous_edges=
  quiesce_stable=0
  quiesce_pass=0
  process_table_ready=0
  while [ "$quiesce_pass" -lt 8 ]; do
    capture_process_table || return 1
    helper_tree_table_result=0
    if ! build_helper_tree "$quiesce_root" "$process_table"; then
      process_table_ready=0
      return 1
    fi
    quiesce_current="$helper_tree_pids"
    quiesce_current_edges="$helper_tree_edges"
    if same_pid_list "$quiesce_previous" "$quiesce_current" && same_pid_list "$quiesce_previous_edges" "$quiesce_current_edges"; then
      quiesce_stable=$((quiesce_stable + 1))
      if [ "$quiesce_stable" -ge 1 ]; then
        return 0
      fi
    else
      quiesce_stable=0
    fi
    quiesce_previous="$quiesce_current"
    quiesce_previous_edges="$quiesce_current_edges"
    for quiesce_pid in $quiesce_current; do
      if kill -STOP "$quiesce_pid" 2>/dev/null; then
        mark_stopped "$quiesce_pid"
      fi
    done
    quiesce_pass=$((quiesce_pass + 1))
  done
  reaper_warn "quiesced descendant snapshot did not reach a fixed point"
  return 1
}
kill_pipeline() {
  kill_root_pid="$1"
  cleanup_active=1
  cleanup_helper_budget=32
  tombstoned_pids=
  stopped_pids=
  capture_identity "$kill_root_pid" "$work_dir/identity.$kill_root_pid"
  snapshot_status="$?"
  if [ "$snapshot_status" -ne 0 ]; then
    reaper_warn "root process identity lookup failed for pid $kill_root_pid"
    kill -9 "$kill_root_pid" 2>/dev/null || true
    return 1
  fi
  revalidate_identity "$kill_root_pid"
  snapshot_status="$?"
  if [ "$snapshot_status" -ne 0 ]; then
    kill -9 "$kill_root_pid" 2>/dev/null || true
    return 1
  fi
  process_table_ready=0
  if [ -n "$pgrep_bin" ]; then
    pgrep_disabled=0
  fi
  # Stop only the validated root process. The timer and helper groups are
  # siblings; a negative group stop could freeze the timer before it reaps
  # its own sleep child.
  if kill -STOP "$kill_root_pid" 2>/dev/null; then
    mark_stopped "$kill_root_pid"
  fi
  capture_quiesced_process_table "$kill_root_pid" || true
  known_parents=" $kill_root_pid"
  known_count=1
  snapshot=
  pass_seen=
  snapshot_error=0
  descendant_lookups=0
  snapshot_pass_parents=$known_parents
  for snapshot_parent in $snapshot_pass_parents; do
    snapshot_branch "$snapshot_parent" 0 || snapshot_error=1
  done
  if [ "$snapshot_error" -ne 0 ]; then
    reaper_warn "descendant snapshot incomplete for pid $kill_root_pid"
    direct_snapshot_fallback
    return 1
  fi
  signal_snapshot
  kill -9 "$kill_root_pid" 2>/dev/null || true
  return 0
}
run_with_timeout() {
  wait_seconds="$1"
  shift
  set -m 2>/dev/null
  "$@" & command_pid=$!
  (
    timer_sleeper=
    cleanup_timer() {
      if [ -n "$timer_sleeper" ]; then
        kill -KILL "$timer_sleeper" 2>/dev/null || true
        wait "$timer_sleeper" 2>/dev/null || true
      fi
    }
    trap 'cleanup_timer; exit 0' HUP INT TERM
    "$sleep_bin" "$wait_seconds" &
    timer_sleeper="$!"
    wait "$timer_sleeper"
    timer_sleeper=
    kill_pipeline "$command_pid"
  ) & timer_pid=$!
  wait "$command_pid" 2>/dev/null
  command_status="$?"
  kill -TERM "$timer_pid" 2>/dev/null || true
  wait "$timer_pid" 2>/dev/null || true
  set +m
  return "$command_status"
}
inspect_projection() {
  {
    "$bin" inspect "$id" 2>/dev/null
    printf '\n__containergo_inspect_rc__%s\n' "$?"
  } | "$awk_bin" -v key="$key" '
    function json_value(line, wanted,    p, rest, quote) {
      p = 1
      while (substr(line, p, 1) == " " || substr(line, p, 1) == "\t") p++
      if (substr(line, p, length(wanted) + 2) != "\"" wanted "\"") return ""
      rest = substr(line, p + length(wanted) + 2)
      if (substr(rest, 1, 1) != ":") return ""
      rest = substr(rest, 2)
      while (substr(rest, 1, 1) == " " || substr(rest, 1, 1) == "\t") rest = substr(rest, 2)
      if (substr(rest, 1, 1) != "\"") return ""
      rest = substr(rest, 2)
      quote = index(rest, "\"")
      if (quote == 0) return ""
      return substr(rest, 1, quote - 1)
    }
    function hex(value, width,    i, c) {
      if (length(value) != width) return 0
      for (i = 1; i <= width; i++) {
        c = substr(value, i, 1)
        if (c !~ /^[0-9a-f]$/) return 0
      }
      return 1
    }
    index($0, "__containergo_inspect_rc__") == 1 {
      rc = $0
      sub(/^__containergo_inspect_rc__/, "", rc)
      next
    }
    {
      value = json_value($0, key)
      if (got == "" && hex(value, 16)) got = value
      value = json_value($0, "Id")
      if (uid == "" && hex(value, 64)) uid = value
    }
    END {
      if (got != "") print "creation=" got
      if (uid != "") print "id=" uid
      print "inspect_rc=" rc
    }
  '
}
valid_docker_id() {
  case "$1" in
    *[!0-9a-f]*) return 1 ;;
  esac
  [ "${#1}" -eq 64 ] 2>/dev/null
}
process_entry() {
  bin="$1"
  sub="$2"
  id="$3"
  creation="$4"
  key="$5"
  target="$id"
  case "$sub" in
    delete|rm) ;;
    *) return 0 ;;
  esac
  if [ "$sub" = "rm" ] && [ -z "$creation" ]; then
    valid_docker_id "$id" || return 0
  fi
  if [ -n "$creation" ]; then
    # Project only bounded fields while inspect is streaming. The raw
    # response may contain credentials, so it never reaches a temp file.
    inspect_fields=$(run_with_timeout 10 inspect_projection "$id") || return 0
    got=
    uid=
    inspect_rc=
    old_ifs=$IFS
    IFS='
'
    for field in $inspect_fields; do
      case "$field" in
        creation=*) [ -n "$got" ] || got=${field#creation=} ;;
        id=*) [ -n "$uid" ] || uid=${field#id=} ;;
        inspect_rc=*) inspect_rc=${field#inspect_rc=} ;;
      esac
    done
    IFS=$old_ifs
    unset inspect_fields
    [ "$inspect_rc" = 0 ] || return 0
    [ "$got" = "$creation" ] || return 0
    if [ "$sub" = "rm" ]; then
      valid_docker_id "$uid" || return 0
      target="$uid"
    fi
  fi
  run_with_timeout "$timeout" "$bin" "$sub" --force "$target" >/dev/null 2>&1 || true
}
echo "$ids" | while IFS= read -r line; do
  # The input loop runs in a pipeline subshell. Re-enable monitor mode
  # there so each helper can receive an isolated process group.
  set -m 2>/dev/null
  [ -z "$line" ] && continue
  id=${line%% *}
  creation=${line#* }
  [ "$id" = "$line" ] && creation=""
  run_with_timeout "$timeout" process_entry "$bin" "$sub" "$id" "$creation" "$key" || true
done
set +f
helper_enumeration=1
run_helper "$work_dir/remove.out" "$work_dir/remove.err" "$rm_bin" -f "$work_dir/pgrep.out" "$work_dir/pgrep.err" "$work_dir/process.table" "$work_dir/process-table.err" "$work_dir"/identity.* || true
set -f
`

const (
	maxReaperSpawnFailures      = 3
	defaultReaperTimeoutSeconds = 30
	reaperCleanupTimeout        = 2 * time.Second
)

// breQuote escapes a literal for the reaper's field matcher, so the label
// key's dots remain literal characters rather than matcher syntax.
func breQuote(s string) string {
	var b strings.Builder
	for _, c := range s {
		if strings.ContainsRune(`\.*[]^$/`, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// creationRE validates the hex generation ID passed to the reaper.
var creationRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

func validReaperID(subcommand, id string) bool {
	return nameRE.MatchString(id) || (subcommand == "rm" && dockerIDRE.MatchString(id))
}

func validateReaperEntry(subcommand, id, creation string) (bool, error) {
	immutableID := subcommand == "rm" && dockerIDRE.MatchString(id)
	if !validReaperID(subcommand, id) {
		return false, fmt.Errorf("reaper: invalid container id %q", id)
	}
	if immutableID {
		return true, nil
	}
	if subcommand == "rm" && creation == "" {
		return false, fmt.Errorf("reaper: Docker name entry %q has no generation", id)
	}
	if creation != "" && !creationRE.MatchString(creation) {
		return false, fmt.Errorf("reaper: invalid creation id %q", creation)
	}
	return false, nil
}

type reaperEntry struct {
	id       string
	creation string
}

type reaper struct {
	binary string
	// subcommand deletes a container: "delete" (Apple) or "rm"
	// (Docker); both take --force.
	subcommand string

	mu            sync.Mutex
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	exited        chan struct{}
	entries       []reaperEntry
	spawnFailures int
	gaveUp        bool
	// timeoutSeconds is an internal test seam; production reapers use
	// defaultReaperTimeoutSeconds.
	timeoutSeconds int
	helperPaths    reaperHelperPaths
	workDir        string
}

func newReaper(binary, subcommand string) *reaper {
	return &reaper{
		binary:         binary,
		subcommand:     subcommand,
		timeoutSeconds: defaultReaperTimeoutSeconds,
	}
}

// register adds a container ID to the reaper's kill list, spawning or
// respawning the reaper process as needed. creation is the generation
// ID from creationLabel; a full immutable Docker ID is normalized to an
// ungenerated entry.
func (r *reaper) register(id, creation string) error {
	immutableID, err := validateReaperEntry(r.subcommand, id, creation)
	if err != nil {
		return err
	}
	if immutableID {
		// A full Docker ID is already immutable; it must not be sent
		// through the name/generation fallback path.
		creation = ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := reaperEntry{id: id, creation: creation}
	r.entries = append(r.entries, entry)
	if r.stdin != nil {
		if r.writeLocked(entry) == nil {
			return nil
		}
	}
	return r.respawnAndReplayLocked()
}

func (r *reaper) writeLocked(e reaperEntry) error {
	if e.creation == "" {
		_, err := io.WriteString(r.stdin, e.id+"\n")
		return err
	}
	_, err := io.WriteString(r.stdin, e.id+" "+e.creation+"\n")
	return err
}

// respawnAndReplayLocked starts a fresh reaper process and re-registers
// every known ID with it. Success resets the consecutive-failure count;
// giving up logs once so a permanently broken reaper is visible.
func (r *reaper) respawnAndReplayLocked() error {
	for r.spawnFailures < maxReaperSpawnFailures {
		if err := r.spawnLocked(); err != nil {
			r.spawnFailures++
			continue
		}
		replayed := true
		for _, e := range r.entries {
			if r.writeLocked(e) != nil {
				replayed = false
				break
			}
		}
		if replayed {
			r.spawnFailures = 0
			return nil
		}
		r.spawnFailures++
	}
	if !r.gaveUp {
		r.gaveUp = true
		log.Printf("container-go: reaper giving up after %d consecutive spawn failures (binary=%q)", maxReaperSpawnFailures, r.binary)
	}
	return errors.New("reaper: giving up after repeated spawn failures")
}

func (r *reaper) spawnLocked() error {
	timeout := r.timeoutSeconds
	if timeout <= 0 {
		timeout = defaultReaperTimeoutSeconds
	}
	helpers := r.helperPaths
	if !helpers.complete() {
		var err error
		helpers, err = trustedReaperHelpers()
		if err != nil {
			return err
		}
	}
	workDir, err := os.MkdirTemp("", "containergo-reaper-")
	if err != nil {
		return err
	}
	psStartField := "lstart"
	if runtime.GOOS == "darwin" {
		psStartField = "stime"
	}
	cmd := exec.Command(
		"/bin/sh", "-c", reaperScript, "containergo-reaper",
		r.binary, r.subcommand, breQuote(creationLabel), strconv.Itoa(timeout),
		workDir, helpers.awk, helpers.pgrep, helpers.ps, helpers.rm, helpers.sleep, psStartField,
	)
	prepareReaperCommand(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = os.RemoveAll(workDir)
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(workDir)
		return err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		_ = os.RemoveAll(workDir)
		close(exited)
	}()
	r.cmd, r.stdin, r.exited = cmd, stdin, exited
	r.helperPaths, r.workDir = helpers, workDir
	return nil
}

// closeStdin hands the reaper the same EOF it would see on parent
// death. Test hook and best-effort shutdown.
func (r *reaper) closeStdin() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stdin != nil {
		_ = r.stdin.Close()
	}
}

// killForTest kills the reaper process group and waits until the child is
// reaped, so fake descendants cannot survive the test and the next write
// deterministically fails. The cleanup is bounded because a test must not
// be able to wedge the whole suite on a stalled pgrep or an expanding tree.
func (r *reaper) killForTest() error {
	r.mu.Lock()
	cmd, exited := r.cmd, r.exited
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), reaperCleanupTimeout)
	defer cancel()
	var cleanupErrors []error
	if cmd != nil {
		if err := killReaperCommand(ctx, cmd); err != nil {
			log.Printf("container-go: reaper cleanup: %v", err)
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	if exited != nil {
		select {
		case <-exited:
		case <-ctx.Done():
			cleanupErrors = append(cleanupErrors, fmt.Errorf("reaper: cleanup timed out: %w", ctx.Err()))
		}
	}
	return errors.Join(cleanupErrors...)
}

var (
	globalReapersMu sync.Mutex
	globalReapers   = map[string]*reaper{}
)

// registerWithGlobalReaper best-effort registers a container with the
// process-wide reaper for its backend binary. Reaper trouble is logged
// but never fails container startup. The reaper needs /bin/sh, so on
// Windows this is a no-op and cleanup relies on the normal paths.
func registerWithGlobalReaper(binary, subcommand, id, creation string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	globalReapersMu.Lock()
	r, ok := globalReapers[binary]
	if !ok {
		r = newReaper(binary, subcommand)
		globalReapers[binary] = r
	}
	globalReapersMu.Unlock()
	if err := r.register(id, creation); err != nil {
		log.Printf("container-go: reaper registration failed (binary=%q): %v", binary, err)
		return err
	}
	return nil
}
