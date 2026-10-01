#!/usr/bin/env bash
# hive-shared-auth.sh — keep ONE credential store working across every spoke.
#
# WHY THIS EXISTS
# ---------------
# The fleet shares one Claude/Gemini/codex login so a single device-flow sign-in
# covers every hive. That sharing is assembled from hand-carved hostPath PVs and
# symlinks, and it has failed silently three separate ways in one week:
#
#   1. A mount that LOOKS fine but is not the shared directory. On this node the
#      same hostPath string can resolve to a different filesystem for a freshly
#      created mount (an existing PV lands on device 0:66, a new one on 0:59 with
#      an empty DirectoryOrCreate). `ls` shows a plausible directory either way.
#      Only writing a file in one spoke and reading it from another proves it.
#   2. Permissions. Token refresh REWRITES the credential file. If the shared dir
#      is not group-writable by `node`, refresh fails and every agent on that
#      backend dies with a login error that looks like an expired subscription.
#   3. `.claude.json` with `theme: null`. hasCompletedOnboarding is true, so
#      nothing looks wrong, but the CLI stops at the theme picker on every launch
#      and the watchdog kill+restarts it forever. Cost reef a whole rotation
#      cycle on 2026-09-06.
#
# So: verify by WRITE-THROUGH, not by looking. Repair what is repairable. Say
# loudly what is not.
#
# USAGE
#   hive-shared-auth.sh check     # verify + report, change nothing
#   hive-shared-auth.sh reconcile # verify, then repair perms/theme

# shellcheck source=hive-lib.sh
. "${HIVE_LIB:-$(dirname "$0")/hive-lib.sh}"
hive_kube_env

set -u

NAMESPACES="${HIVE_SHARED_AUTH_NAMESPACES:-hive hive-reef hive-hanthor}"
# Directories under $HOME expected to be the SAME storage in every namespace.
SHARED_DIRS="${HIVE_SHARED_AUTH_DIRS:-.claude .gemini .codex}"
PRIMARY="${HIVE_SHARED_AUTH_PRIMARY:-hive}"
# The agents' home, spelled out. Do NOT use $HOME here: `kubectl exec` runs as
# root and $HOME is /root, so every check silently inspects the wrong directory
# and reports a healthy fleet as broken (and a broken one as repaired).
AHOME="${HIVE_AGENT_HOME:-/data/home}"
ACTION="${1:-check}"
case "$ACTION" in check|reconcile) ;; *) echo "usage: $0 check|reconcile" >&2; exit 2;; esac

rc=0
pod_of() { hive_pod "$1"; }
kx() { timeout "${HIVE_EXEC_TIMEOUT:-120}" kubectl exec -n "$1" "$2" -- sh -c "$3" 2>/dev/null; }

PPOD=$(pod_of "$PRIMARY")
[ -n "$PPOD" ] || { echo "ERROR: no hive pod in primary namespace $PRIMARY" >&2; exit 1; }

# ── Write-through check ─────────────────────────────────────────────────
# A marker written in the primary must appear in every other spoke. The marker
# NAME is unique per run: the tuna-os hive-operator's SharedAuth controller
# (ns hive-system, Shadow mode) probes the same directories with the same
# `.shared-auth-probe` filename, and whichever wrote last made the other read
# a foreign stamp — the "NOT SHARED" alarms of 2026-09-24 were that race, not
# a private copy (same device 0:66 and inode in all three pods).
#
# Batched: one exec writes every marker, one exec per spoke reads them all,
# one exec cleans up. It used to be (1 write + N reads + 1 rm) per directory,
# plus three more per spoke below — ~21 execs at 10-35 s each on the loaded
# hive node.
stamp="probe-$$-$(od -An -N4 -tu4 < /dev/urandom 2>/dev/null | tr -d ' ' || date +%s)"
marker=".shared-auth-probe.$stamp"
wfail=$(kx "$PRIMARY" "$PPOD" "for d in $SHARED_DIRS; do printf '%s' '$stamp' > $AHOME/\$d/$marker || echo \"\$d\"; done")
[ -n "$wfail" ] && { printf '%-10s %-9s PRIMARY NOT WRITABLE — cannot verify\n' "$(echo $wfail)" "$PRIMARY"; rc=1; }
for ns in $NAMESPACES; do
  [ "$ns" = "$PRIMARY" ] && continue
  pod=$(pod_of "$ns")
  if [ -z "$pod" ]; then printf '%-10s %-14s no hive pod — skipped\n' "*" "$ns"; continue; fi
  seen=$(kx "$ns" "$pod" "for d in $SHARED_DIRS; do printf '%s %s\n' \"\$d\" \"\$(cat $AHOME/\$d/$marker 2>/dev/null)\"; done")
  for d in $SHARED_DIRS; do
    v=$(printf '%s\n' "$seen" | awk -v d="$d" '$1==d {print $2}')
    if [ "$v" = "$stamp" ]; then
      printf '%-10s %-14s SHARED ok\n' "$d" "$ns"
    elif [ -z "$seen" ]; then
      printf '%-10s %-14s could not read (exec failed) — not judged\n' "$d" "$ns"
    else
      printf '%-10s %-14s NOT SHARED — this spoke has a private copy; a login here will not propagate\n' "$d" "$ns"
      rc=1
    fi
  done
done
kx "$PRIMARY" "$PPOD" "for d in $SHARED_DIRS; do rm -f $AHOME/\$d/$marker; done" >/dev/null || true

# ── Credential health + repairs, ONE exec per spoke ─────────────────────
# Per spoke: claude token present? .claude.json theme set (theme:null stops
# the CLI at the picker forever)? On reconcile also: repair the theme, make
# the shared dirs group-writable (token refresh REWRITES the credential file;
# without group write it fails and presents as an expired subscription), and
# drop a broken agy statusLine.
#
# The statusLine repair (2026-09-24): the shared antigravity-cli/settings.json
# carried {"statusLine":{"type":"command","command":"/status"}} — almost
# certainly the old openai pane probe typing `/status` into an agy pane (it
# chose agents by MODEL name, and hanthor/telemetry was agy running
# gpt-5.6-luna). Every agy launch then printed "Statusline error: sh: 1:
# /status: not found". Only that exact broken value is removed.
for ns in $NAMESPACES; do
  pod=$(pod_of "$ns"); [ -n "$pod" ] || continue
  # shellcheck disable=SC2016
  out=$(timeout "${HIVE_EXEC_TIMEOUT:-120}" kubectl exec -n "$ns" "$pod" -- sh -c '
    H="$1"; ACT="$2"; DIRS="$3"
    t=$(jq -r "if ((.claudeAiOauth.accessToken // \"\") == \"\") then \"EMPTY\" else \"ok\" end" "$H/.claude/.credentials.json" 2>/dev/null)
    echo "token ${t:-unreadable}"
    th=$(jq -r ".theme // \"null\"" "$H/.claude.json" 2>/dev/null)
    if [ "${th:-null}" = null ]; then
      if [ "$ACT" = reconcile ]; then
        f="$H/.claude.json"
        if [ ! -f "$f" ]; then
          printf "%s" "{\"theme\":\"dark\",\"hasCompletedOnboarding\":true}" > "$f" && chgrp node "$f" && chmod 664 "$f" \
            && echo "theme repaired" || echo "theme repair-failed"
        else
          tmp=$(mktemp) && jq ".theme = \"dark\" | .hasCompletedOnboarding = true" "$f" > "$tmp" \
            && cat "$tmp" > "$f" && rm -f "$tmp" && echo "theme repaired" || echo "theme repair-failed"
        fi
      else echo "theme unset"; fi
    fi
    sf="$H/.gemini/antigravity-cli/settings.json"
    if [ -f "$sf" ] && [ "$(jq -r ".statusLine.command // empty" "$sf" 2>/dev/null)" = "/status" ]; then
      if [ "$ACT" = reconcile ]; then
        tmp=$(mktemp) && jq "del(.statusLine)" "$sf" > "$tmp" && cat "$tmp" > "$sf" && rm -f "$tmp" \
          && echo "statusline repaired" || echo "statusline repair-failed"
      else echo "statusline broken"; fi
    fi
    if [ "$ACT" = reconcile ]; then
      for d in $DIRS; do p="$H/$d"; [ -e "$p" ] || continue
        chgrp -R node "$p" 2>/dev/null; chmod -R g+rwX "$p" 2>/dev/null; done
    fi' sh "$AHOME" "$ACTION" "$SHARED_DIRS" 2>/dev/null)
  case "$(printf '%s\n' "$out" | awk '$1=="token"{print $2}')" in
    ok)    printf '%-10s %-14s claude token present\n' "creds" "$ns" ;;
    EMPTY) printf '%-10s %-14s CLAUDE TOKEN EMPTY — needs `claude auth login` (one login covers the fleet)\n' "creds" "$ns"; rc=1 ;;
    *)     printf '%-10s %-14s claude credential unreadable\n' "creds" "$ns"; rc=1 ;;
  esac
  case "$out" in *"theme repaired"*) printf '%-10s %-14s theme was unset -> set to dark\n' theme "$ns" ;; esac
  case "$out" in *"theme repair-failed"*) printf '%-10s %-14s theme unset and REPAIR FAILED\n' theme "$ns"; rc=1 ;; esac
  case "$out" in *"theme unset"*) printf '%-10s %-14s THEME UNSET — CLI will stop at the picker; run reconcile\n' theme "$ns"; rc=1 ;; esac
  case "$out" in *"statusline repaired"*) printf '%-10s %-14s broken agy statusLine (/status) removed\n' agy "$ns" ;; esac
  case "$out" in *"statusline broken"*) printf '%-10s %-14s agy statusLine is "/status" (broken) — run reconcile\n' agy "$ns" ;; esac
  case "$out" in *"statusline repair-failed"*) printf '%-10s %-14s statusLine REPAIR FAILED\n' agy "$ns"; rc=1 ;; esac
done

[ "$rc" = 0 ] && echo "shared-auth: all spokes consistent" || echo "shared-auth: PROBLEMS ABOVE"
exit $rc
