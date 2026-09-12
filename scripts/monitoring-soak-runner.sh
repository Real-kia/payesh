#!/bin/sh
set -eu

usage() {
  printf '%s\n' 'Usage: monitoring-soak-runner.sh start|status|stop --binary /absolute/path [--state-dir /absolute/path] [--duration 24h] [--interval 1s]'
}

[ "$#" -ge 1 ] || { usage >&2; exit 2; }
action=$1
shift
binary=
state_dir=/var/lib/payesh-acceptance/monitoring-soak
duration=24h
interval=1s
while [ "$#" -gt 0 ]; do
  case "$1" in
    --binary) [ "$#" -ge 2 ] || { usage >&2; exit 2; }; binary=$2; shift 2 ;;
    --state-dir) [ "$#" -ge 2 ] || { usage >&2; exit 2; }; state_dir=$2; shift 2 ;;
    --duration) [ "$#" -ge 2 ] || { usage >&2; exit 2; }; duration=$2; shift 2 ;;
    --interval) [ "$#" -ge 2 ] || { usage >&2; exit 2; }; interval=$2; shift 2 ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
done

case "$state_dir" in /*) ;; *) printf 'state directory must be absolute\n' >&2; exit 2 ;; esac
case "$state_dir" in /|/tmp|/var|/var/lib) printf 'state directory is too broad\n' >&2; exit 2 ;; esac
pid_file=$state_dir/pid
report_file=$state_dir/report.json
error_file=$state_dir/error.log

owned_pid() {
  [ -f "$pid_file" ] || return 1
  pid=$(sed -n '1p' "$pid_file")
  case "$pid" in ''|*[!0-9]*) return 1 ;; esac
  kill -0 "$pid" 2>/dev/null || return 1
  command=$(ps -p "$pid" -o command= 2>/dev/null || true)
  case "$command" in *monitoring-soak*) return 0 ;; *) return 1 ;; esac
}

case "$action" in
  start)
    [ -n "$binary" ] || { printf '%s\n' '--binary is required for start' >&2; exit 2; }
    [ -x "$binary" ] || { printf 'binary is not executable: %s\n' "$binary" >&2; exit 2; }
    if owned_pid; then printf 'monitoring_soak=running pid=%s\n' "$pid"; exit 0; fi
    install -d -m 0700 "$state_dir"
    : >"$report_file"
    : >"$error_file"
    nohup "$binary" -duration "$duration" -interval "$interval" -batch-size 1 -prune-interval 5m -retention-age 1h -quiet >"$report_file" 2>"$error_file" &
    pid=$!
    printf '%s\n' "$pid" >"$pid_file"
    sleep 1
    kill -0 "$pid" 2>/dev/null || { printf 'monitoring soak failed to start\n' >&2; cat "$error_file" >&2; exit 1; }
    printf 'monitoring_soak=started pid=%s duration=%s report=%s\n' "$pid" "$duration" "$report_file"
    ;;
  status)
    if owned_pid; then
      elapsed=$(ps -p "$pid" -o etime= 2>/dev/null | tr -d ' ')
      printf 'monitoring_soak=running pid=%s elapsed=%s\n' "$pid" "$elapsed"
      exit 0
    fi
    if [ -s "$report_file" ]; then
      attempted=$(sed -n 's/.*"attempted_samples":\([0-9][0-9]*\).*/\1/p' "$report_file")
      inserted=$(sed -n 's/.*"inserted_samples":\([0-9][0-9]*\).*/\1/p' "$report_file")
      if grep -q '"format":"payesh.monitoring-soak.v1"' "$report_file" && grep -q '"clean_teardown":true' "$report_file" && grep -q '"retention_observed":true' "$report_file" && grep -q '"duplicate_samples":0' "$report_file" && grep -q '"ingest_errors":0' "$report_file" && grep -q '"prune_errors":0' "$report_file" && grep -q '"storage_errors":0' "$report_file" && [ -n "$attempted" ] && [ "$attempted" = "$inserted" ]; then
        printf 'monitoring_soak=passed report=%s\n' "$report_file"
        cat "$report_file"
        exit 0
      fi
      printf 'monitoring_soak=failed report=%s\n' "$report_file" >&2
      cat "$report_file" >&2
      [ ! -s "$error_file" ] || cat "$error_file" >&2
      exit 1
    fi
    printf 'monitoring_soak=not-running\n'
    [ ! -s "$error_file" ] || cat "$error_file" >&2
    exit 1
    ;;
  stop)
    if owned_pid; then
      kill "$pid"
      for _ in 1 2 3 4 5; do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
      kill -0 "$pid" 2>/dev/null && { printf 'monitoring soak did not stop cleanly\n' >&2; exit 1; }
      printf 'monitoring_soak=stopped pid=%s\n' "$pid"
    else
      printf 'monitoring_soak=not-running\n'
    fi
    ;;
  *) usage >&2; exit 2 ;;
esac
