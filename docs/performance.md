# Performance

Measurements behind the memory defaults, with the method so they can be
repeated. All runs used a throwaway instance (`SONARLESS_INSTANCE=perftest`,
its own `SONARLESS_HOME` and ports), never a real install. The scripts are in
[`scripts/perf/`](../scripts/perf).

Measured on 2026-11-25: Linux 6.8, 14 cores, 31 GiB RAM, Docker 29.10, SonarQube
`26.6.0.123539-community` (Java 25), `sonarsource/sonarqube-mcp:latest` built
2026-11-20 (a jlinked Java 21.0.13), sonar-scanner-cli 12.4.

## Summary

| Question | Result | Default |
|---|---|---|
| Is a 256m Elasticsearch heap enough for a large repo? | Yes. No OutOfMemoryError, no full GC, live old generation at most 72 MiB; same compute-engine time as 384m | `SONARLESS_SEARCH_HEAP=256m` kept |
| Where does the MCP JVM's memory outside the heap go? | Class metadata (71 MiB) and symbols (34 MiB) for 14.5k loaded classes; code cache 13 MiB; 82 threads 8 MiB | `SONARLESS_MCP_JAVA_OPTS` kept |
| Does AppCDS help the MCP JVM? | Yes: ready in 2.0 s instead of 4.1 s, java RSS 28 MiB lower | not wired in; see [AppCDS](#appcds-for-the-mcp-jvm) |

## Elasticsearch heap: 256m against 384m

**Method.** Scan a copy of the largest local repo the scanner actually
analyses (Workit at `bc1d174b`: 1508 files indexed, 972 analysed, 251,450 lines
of code, 3363 issues), once on a fresh server and again as a re-analysis, at
each heap. During each scan `jstat -gc` samples the Elasticsearch JVM every
second and `docker stats` samples the server container. After the scans, a
burst of 60 issue searches with facets (the queries the MCP issue tools send)
runs against the same index. The OutOfMemoryError, GC-overhead and
circuit-breaker counts come from `es.log`, `ce.log` and `web.log`.

| Run | Scan wall | Scanner total | Compute engine | Young GCs | Full GCs | Heap used, peak | Old gen, peak | Server container, peak |
|---|---|---|---|---|---|---|---|---|
| 256m, fresh | 190.8 s | 2:46 | 21.8 s | +1 (5 ms) | 0 | 164 MiB | 72 MiB | 2446 MiB |
| 256m, re-analysis | 174.9 s | 2:42 | 10.6 s | +1 (4 ms) | 0 | 196 MiB | 72 MiB | 2623 MiB |
| 256m, re-analysis after a recreate | 163.8 s | 2:26 | 15.4 s | +1 (6 ms) | 0 | 185 MiB | 42 MiB | 1989 MiB |
| 384m, fresh | 172.7 s | 2:30 | 20.9 s | +1 (4 ms) | 0 | 293 MiB | 120 MiB | 2494 MiB |
| 384m, re-analysis | 156.3 s | 2:23 | 11.1 s | 0 | 0 | 230 MiB | 120 MiB | 2659 MiB |
| 256m, 60 issue searches | 16.4 s | | | +2 (7 ms) | 0 | 211 MiB | 72 MiB | |
| 384m, 60 issue searches | 15.7 s | | | +2 (6 ms) | 0 | 322 MiB | 120 MiB | |

No run logged an OutOfMemoryError, a GC-overhead error or a tripped circuit
breaker. Elasticsearch spent under 40 ms in GC across a whole scan at either
size. The difference in wall time sits in the scanner's JavaScript/TypeScript
sensor (149, 147 and 129 s at 256m; 135 and 130 s at 384m), which runs in the
scanner container and never calls Elasticsearch: the third 256m run was as
fast as the 384m runs. The work Elasticsearch takes part in, the compute engine,
took the same time at both sizes. A larger heap only lets G1 leave more
garbage before it collects, which is why the peaks rise with the heap.

Elasticsearch's memory outside the heap (class metadata, code, threads) was
142 to 158 MiB at both sizes, and it maps index files outside the heap through
the page cache, so the heap is not its whole footprint.

## MCP JVM native memory

**Method.** `scripts/perf/mcp-nmt.sh` runs the MCP container with the default
`SONARLESS_MCP_JAVA_OPTS` plus `-XX:NativeMemoryTracking=summary`, runs an
agent-like session (initialize, `tools/list`, 12 tool calls: projects, quality
gate, measures, six pages of issues, three rules), then reads
`VM.native_memory summary`. The image ships only `java`, so `jcmd` runs from a
JDK 21 container that shares the MCP container's PID namespace and user
(`--pid container:<mcp> --user 100:101`).

| Area (committed) | Default | With AppCDS |
|---|---|---|
| Java heap | 64.0 MiB | 64.0 MiB |
| Metaspace (non-class metadata) | 71.0 MiB | 4.1 MiB |
| Symbol | 34.3 MiB | 2.3 MiB |
| Code (C1 only) | 13.2 MiB | 11.8 MiB |
| Class space and class malloc | 11.2 MiB | 2.7 MiB |
| Thread (82 threads, 512k stacks) | 8.2 MiB | 8.0 MiB |
| Shared class space (the mapped archive) | | 99.0 MiB |
| Internal, GC, compiler, other | 2.4 MiB | 2.4 MiB |
| NMT's own bookkeeping | 4.5 MiB | 1.4 MiB |
| Total committed | 209.2 MiB | 196.3 MiB |

At the moment of the read the JVM had 14,507 classes loaded (13,474 instance
classes and 1,033 array classes, NMT's count). They cost more than the heap:
metadata and symbols together are 105 MiB.

`MaxMetaspaceSize=128m` caps class metadata and class space together. The
table shows committed memory; NMT also reports what is used inside it:
metadata used 70.0 MiB (70.6 MiB committed; the table's 71.0 MiB adds the
category's own malloc) plus class space used 9.2 MiB (9.8 MiB committed) make
the 79 MiB of the cap in use. The cap only limits growth, so lowering it frees
nothing and only brings an OutOfMemoryError in Metaspace closer. The
thread count comes from the server's own pools, not from a JVM option.

Apart from AppCDS, no option to shrink metadata or symbols was measured. The
size follows the number of classes the server loads, and AppCDS is the one
change measured that moves both out of committed native memory: into a
read-only mapped file, of which only the pages touched are resident.

## AppCDS for the MCP JVM

**Method.** The image's jlinked JDK has no base CDS archive, so
`-XX:+AutoCreateSharedArchive` only warns
(`ArchiveClassesAtExit is unsupported when base CDS archive is not loaded`).
A static archive works (`scripts/perf/mcp-cds.sh`): a training run with
`-XX:DumpLoadedClassList` and the agent session writes a class list of 12,965
lines (the classes the JVM may archive, lambda entries included, recorded up
to the training run's exit), then `java -Xshare:dump` with the same options
writes a 101 MiB archive in about 5 s. In a run with the archive,
`-Xlog:class+load` logged 13,299 instance classes loaded up to the container's
stop, 12,443 of them from the archive. The three counts differ because each
is taken at a different point and counts different things: the list leaves
out classes the JVM cannot archive, the log counts instance classes only, and
NMT's 14,507 includes array classes. Each run starts a fresh container and measures
`docker run` to `/health` answering 200, then the java RSS (`VmRSS`) and the
container's memory (`docker stats`) 10 s after ready and after the agent
session.

| | Ready (median) | Java RSS idle | Java RSS after session | Container after session |
|---|---|---|---|---|
| Default options, 7 runs | 4.1 s (3.8 to 7.5) | 219 MiB | 231 MiB | 207 MiB |
| Default options + AppCDS, 6 runs | 2.0 s (1.4 to 2.5) | 191 MiB | 201 MiB | 171 MiB |

Tool latency did not change (every session's 12 calls finished in under 2 s).

AppCDS is a clear win, but it is a feature rather than a new default value: the
archive must be built for each MCP image (a training start and a dump, about
10 s, after every image update), stored somewhere that survives container
recreation (about 100 MiB), and passed in `JAVA_TOOL_OPTIONS`. With a stale
archive (`scripts/perf/mcp-stale.sh`: the same jar with another timestamp
mounted over it) the JVM logs
`[cds] ... timestamp has changed`, runs without the archive and lands on the
default numbers (ready 3.3 s, RSS 218 MiB), so a stale archive costs startup
time, not correctness. Wiring it into sonarless-mcp is left as a
follow-up.

## Repeat the measurements

Sourcing `scripts/perf/env.sh` points `HOME`, the `XDG_*` dirs and every
`SONARLESS_*` setting of the current shell at `PERF_DIR`, so run everything in
a subshell. Each script also checks on its own that `SONARLESS_INSTANCE`
contains `perftest` and that `SONARLESS_HOME` is under `PERF_DIR`, and stops
before any docker call if not.

```sh
export PERF_DIR=$(mktemp -d)
go build -buildvcs=false -o "$PERF_DIR/sonarless-mcp" ./cmd/sonarless-mcp
mkdir "$PERF_DIR/repo"           # scans write .scannerwork: scan a copy
git -C <repo> archive HEAD | tar -x -C "$PERF_DIR/repo"
(
  . scripts/perf/env.sh          # perftest instance, ports 19233 to 19236
  sl start
  scripts/perf/es-scan.sh es256 "$PERF_DIR/repo"
  scripts/perf/es-burst.sh es256-burst <project key>
  SONARLESS_SEARCH_HEAP=384m sl start            # recreates the server, data kept
  SONARLESS_SEARCH_HEAP=384m scripts/perf/es-scan.sh es384 "$PERF_DIR/repo"
  opts='-Xmx64m -XX:+UseSerialGC -XX:TieredStopAtLevel=1 -XX:ReservedCodeCacheSize=32m -XX:MaxMetaspaceSize=128m -Xss512k'
  scripts/perf/mcp-run.sh base "$opts"
  scripts/perf/mcp-nmt.sh nmt "$opts"
  scripts/perf/mcp-cds.sh "$opts"
  scripts/perf/mcp-run.sh cds "$opts -XX:SharedArchiveFile=/cds/app.jsa" -v "$PERF_DIR/cds:/cds:ro"
  scripts/perf/mcp-nmt.sh nmt-cds "$opts -XX:SharedArchiveFile=/cds/app.jsa" -v "$PERF_DIR/cds:/cds:ro"
  scripts/perf/mcp-stale.sh "$opts"
)
```

Afterwards remove only the perftest resources: the containers `perftest-server`
and `perftest-mcpx` (with `docker rm -fv`, which also drops the server's
anonymous `/opt/sonarqube/temp` volume), the network `perftest` and the
volumes `perftest-server-<version>-{data,extensions,logs}`.
