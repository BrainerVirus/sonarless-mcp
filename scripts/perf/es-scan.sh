#!/usr/bin/env bash
# es-scan.sh <label> <repo-copy>: scan a COPY of a repo on the perftest instance
# (the scanner writes .scannerwork into it) while sampling the SonarQube
# Elasticsearch JVM with jstat and the server container's memory, then print one
# summary line: scan time, compute engine time, GC counts and heap peaks.
# Source env.sh first; the heap under test is SONARLESS_SEARCH_HEAP.
set -euo pipefail
: "${PERF_DIR:?source scripts/perf/env.sh first}"
# Re-check the isolation here, not only in env.sh: never touch a real install.
case "${SONARLESS_INSTANCE:-}" in
*perftest*) ;;
*)
  echo "$(basename "$0"): SONARLESS_INSTANCE must contain perftest; source scripts/perf/env.sh first" >&2
  exit 1
  ;;
esac
case "${SONARLESS_HOME:-}" in
"$PERF_DIR"/*) ;;
*)
  echo "$(basename "$0"): SONARLESS_HOME must be under PERF_DIR; source scripts/perf/env.sh first" >&2
  exit 1
  ;;
esac
label=$1 repo=$2
L="$PERF_DIR/logs/$label"
C="$SONARLESS_INSTANCE-server"
mkdir -p "$L"

es=$(docker exec "$C" sh -c "ps -eo pid,args | grep -- '-Des.networkaddress' | grep -v grep | awk '{print \$1}'")
docker exec "$C" sh -c "ps -o args= -p $es" | tr ' ' '\n' | grep -E '^-X' >"$L/es-jvm-flags.txt"
docker exec "$C" jstat -gc "$es" 1000 >"$L/jstat-gc.txt" 2>&1 &
js=$!
(while true; do docker stats --no-stream --format '{{.MemUsage}}' "$C"; sleep 2; done) >"$L/docker-mem.txt" 2>&1 &
ds=$!

s=$(date +%s.%N)
rc=0
"$SONARLESS_BIN" scan "$repo" >"$L/scan.out" 2>&1 || rc=$?
e=$(date +%s.%N)
sleep 3
kill "$js" "$ds" 2>/dev/null || true
wait "$js" "$ds" 2>/dev/null || true
docker exec "$C" pkill -f 'jstat -g[c]' || true

curl -s -u "${SONARLESS_ADMIN_USER:-admin}:${SONARLESS_ADMIN_PASS:-admin}" \
  "http://localhost:$SONARLESS_SERVER_PORT/api/ce/activity?ps=1" >"$L/ce-activity.json"
oom=$(docker exec "$C" sh -c 'grep -v "JVM arguments" /opt/sonarqube/logs/es.log /opt/sonarqube/logs/ce.log /opt/sonarqube/logs/web.log | grep -ciE "OutOfMemoryError|GC overhead|CircuitBreakingException" || true')

python3 -I - "$L" "$label" "$rc" "$(echo "$e - $s" | bc)" "$oom" <<'PY'
import json, re, sys
L, label, rc, wall, oom = sys.argv[1:]
# jstat -gc columns: S0C S1C S0U S1U EC EU OC OU MC MU CCSC CCSU YGC YGCT FGC FGCT CGC CGCT GCT
rows = [[float(x) for x in l.split()] for l in open(L + "/jstat-gc.txt")
        if len(l.split()) == 19 and not l.lstrip().startswith("S0C")]
f, l = rows[0], rows[-1]
used = lambda r: r[2] + r[3] + r[5] + r[7]
mem = []
for x in open(L + "/docker-mem.txt"):
    m = re.match(r"([\d.]+)(GiB|MiB)", x)
    if m:
        mem.append(float(m.group(1)) * (1024 if m.group(2) == "GiB" else 1))
tasks = json.load(open(L + "/ce-activity.json")).get("tasks", [])
ce = tasks[0].get("executionTimeMs") if tasks else None
total = re.search(r"Total time: (\S+)", open(L + "/scan.out").read())
print(f"{label} rc={rc} wall_s={float(wall):.1f} scanner={total.group(1) if total else '?'} ce_ms={ce} "
      f"young_gc=+{l[12]-f[12]:.0f}({l[13]-f[13]:.3f}s) full_gc=+{l[14]-f[14]:.0f}({l[15]-f[15]:.3f}s) "
      f"conc_gc=+{l[16]-f[16]:.0f}({l[17]-f[17]:.3f}s) heap_peak={max(map(used, rows))/1024:.0f}MiB "
      f"old_peak={max(r[7] for r in rows)/1024:.0f}MiB container_peak={max(mem) if mem else 0:.0f}MiB oom_lines={oom}")
PY
