#!/usr/bin/env bash
# es-burst.sh <label> <projectKey>: 60 issue searches with facets, the queries
# agents' issue tools send, while sampling the Elasticsearch JVM; prints the
# burst time, HTTP codes and GC counts. Source env.sh first.
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
label=$1 key=$2
L="$PERF_DIR/logs/$label"
C="$SONARLESS_INSTANCE-server"
mkdir -p "$L"
es=$(docker exec "$C" sh -c "ps -eo pid,args | grep -- '-Des.networkaddress' | grep -v grep | awk '{print \$1}'")
docker exec "$C" jstat -gc "$es" 500 >"$L/jstat-gc.txt" 2>&1 &
js=$!
auth="${SONARLESS_ADMIN_USER:-admin}:${SONARLESS_ADMIN_PASS:-admin}"
s=$(date +%s.%N)
codes=""
for i in $(seq 1 20); do
  for q in "ps=500&p=$(((i % 5) + 1))" \
    "ps=100&facets=rules,severities,types,files,tags,author,directories&additionalFields=_all" \
    "ps=500&issueStatuses=OPEN,CONFIRMED&s=FILE_LINE&facets=impactSoftwareQualities,cleanCodeAttributeCategories"; do
    codes="$codes $(curl -s -o /dev/null -w '%{http_code}' -u "$auth" "http://localhost:$SONARLESS_SERVER_PORT/api/issues/search?components=$key&$q")"
  done
done
e=$(date +%s.%N)
sleep 1
kill "$js" 2>/dev/null || true
wait "$js" 2>/dev/null || true
docker exec "$C" pkill -f 'jstat -g[c]' || true
python3 -I - "$L" "$label" "$(echo "$e - $s" | bc)" "$codes" <<'PY'
import collections, sys
L, label, secs, codes = sys.argv[1:]
rows = [[float(x) for x in l.split()] for l in open(L + "/jstat-gc.txt")
        if len(l.split()) == 19 and not l.lstrip().startswith("S0C")]
f, l = rows[0], rows[-1]
print(f"{label} burst_s={float(secs):.1f} http={dict(collections.Counter(codes.split()))} "
      f"young_gc=+{l[12]-f[12]:.0f}({l[13]-f[13]:.3f}s) full_gc=+{l[14]-f[14]:.0f} "
      f"heap_peak={max(r[2]+r[3]+r[5]+r[7] for r in rows)/1024:.0f}MiB old_peak={max(r[7] for r in rows)/1024:.0f}MiB")
PY
