# Local benchmark report — 2026-10-16

This report records short developer-machine experiments, not production capacity or an SLO. Machine-readable runs live in [local-2026-10-16-final](local-2026-10-16-final/summary.json). Each run retains every offered request, measured delivery/attempt, receiver observation and queue/resource sample. No credentials or event bodies are retained.

## Reproduce

From the project root with the existing local development `.env`, Python 3, Docker and pinned Go available:

```sh
make dev-rate-up
make benchmark BENCHMARK_OUTPUT=benchmarks/my-new-run
make benchmark-plans BENCHMARK_OUTPUT=benchmarks/my-new-plans
make benchmark-profile
make security-check
```

Output directories must be new, preserving earlier runs. Redis starts for integration testing convenience but benchmarks explicitly disable admission. `make benchmark` builds normal binaries, seeds new tenants/endpoints via the API in disposable generated schemas, runs each workload and drops its schemas. The planner fixture directly seeds consistent synthetic event bytes/hashes and two endpoint delivery sets only within that generated schema. Guards reject non-development profiles, foreign users/databases/ports, non-loopback database URLs and pre-scoped database URLs. No production data or existing tenant is reused, and repeated runs do not overwrite application data. The existing helper provisions a separate receiver schema too, although the load receiver itself is a controlled Python HTTP fixture.

For custom duration/repetitions, invoke `python3 scripts/benchmark.py --output benchmarks/my-custom-run --duration 30 --repetitions 3`. Supported bounds are 2–60 seconds and 1–5 repetitions. The crash probe always uses a separate 2-second/two-event offer phase and one repetition. Run on an otherwise quiet machine; do not run tests/profilers concurrently with load. The initial incomplete run is explicitly retained and excluded; see [its explanation](local-2026-10-16/INCOMPLETE.md).

## Environment and configuration

The exact [environment capture](local-2026-10-16-final/environment.json) records macOS 26.6.2 arm64, an Apple M4 MacBook Air with 10 cores (4 performance/6 efficiency), 16 GB RAM, Go 1.27.2, PostgreSQL 18.6, Docker Desktop 4.80.0 / Engine 29.6.1. Docker's VM reports 10 CPUs and 8,321,515,520 bytes memory. sysctl memory lookup was unavailable in this sandbox; hardware profiler supplied the 16 GB figure. Hardware serial/UUID information is excluded.

The Python open-loop generator, one Go API, two Go workers (four slots each) and threaded Python receiver share macOS loopback; PostgreSQL is reached over Docker Desktop's published TCP port. Each Go process has a 10-connection pool (30 combined configured maximum), tenant quota four, backlog cap 10,000, poll 20 ms, HTTP timeout 10 s, lease 30 s, and normal finite retry policy. Four tenants receive round-robin events. Bodies are 1,070 request bytes (1,024-character synthetic data). Fan-out is one endpoint per event except the ten-endpoint workload. Outbound connections use the real protected worker transport and signing path; the controlled receiver does not verify HMAC or perform a business database transaction. Signed transactional receiver correctness remains separately tested; its production performance is not measured here. Rate limiting and tracing are explicitly off; process metrics/logging remain on. No public network/TLS latency is included.

## Method

Healthy rates increase 10 -> 50 -> 100 events/sec, stopping escalation on admission/generator failures or unfinished work. Each regular workload has two 10-second measured offer phases. Healthy/slow/fan-out runs use 20 separate warmup events nominally spaced over two seconds, then wait for warmup to drain; failure/outage runs have no warmup. Actual warmup takes longer than two seconds because API response time is additional; it is not a precisely timed warmup interval. Warmup rows are tagged and excluded from request/delivery latency cohorts; final global status snapshots include them.

Offers are scheduled from monotonic absolute deadlines independently of prior responses. At most 64 requests may be outstanding; saturation becomes an explicit dropped offer, never a growing hidden queue. Raw generator lag and both request-start-to-response and scheduled-arrival-to-response durations expose coordinated-omission risk. Late starts still issue their offers. Transport failures are uncertain acceptance, not proof that no transaction committed. The cohort query uses measured idempotency keys, including durable work whose response could be lost.

Accepted requests/sec counts successful client responses; a separate count records those completed before the offer window ends. Attempt/success rates use measured-cohort PostgreSQL started/finished timestamps within the host wall-clock offer window, not receiver packet counts. This requires synchronized host/VM clocks; no clock-offset calibration is claimed. Delivery latency is DB-created -> DB-committed acknowledgment; HTTP latency is the worker's integer-millisecond observation and can read zero below 1 ms. First queue delay includes only each delivery's first attempt. Percentiles use nearest rank; raw p50/p95/p99 exist for every cohort. Success-only acknowledgment percentiles are always accompanied by delivered/dead-letter/unfinished counts; empty success cohorts return null. Retries are additional attempts, not additional accepted events.

After offering stops, outstanding API responses settle and the queue is observed for at most 45 seconds. `drain_s_after_responses` includes polling/SQL observation overhead and starts after pending responses; the wall-clock offer-to-final interval is therefore slightly longer. Window-end backlog and sampled queue growth are retained; a window snapshot is fetched just after the offer deadline and is not an atomic snapshot at its exact instant. Sampling uses Docker CLI/psql subprocesses, which add load, and resource samples are roughly 1.5-second intervals rather than a precise cadence. Docker stats provide database CPU/memory observations. Aggregate child CPU/max RSS also include migration/admin/Docker clients; they are not per-process API/worker measurements. Per-process Go CPU/RSS under load and generator CPU are unavailable here and are not estimated.

## Results

The generated [results table](results.md) lists each repetition, including backlog, drain, failures and censored work. At 100 offered events/sec, both healthy repetitions accepted all 1,000 events, with ingestion p95 5.44–5.55 ms and acknowledgment p95 14.67–14.79 ms, zero generator/admission failures, unfinished deliveries or lease expirations. These two ten-second observations do not establish maximum sustainable throughput or a long-running service objective.

Slow receivers deliberately sleep 200 ms. At 50 offered events/sec the eight worker slots complete about 37 deliveries/sec during the window; backlog grows to 125–127 and drains afterward. This is expected concurrency saturation, not database ingestion failure. All 500 deliveries eventually acknowledge per repetition. Permanent HTTP 400 workloads accept events but dead-letter every delivery and have zero successes; acknowledgment latency is null. The HTTP 503 outage lasts the first half of each offer phase, then restores automatically; retries and recovery delay are retained. The fan-out experiment offers 10 events/sec but creates 100 deliveries/sec, distinguishing event acceptance from delivery throughput.

The separate crash experiment kills one worker after real receiver entry, releases the blocked receiver and restarts the worker with the normal 30-second lease. Its measured recovery and abandoned-attempt counts are in the results table/raw file. This measures recovery of two ambiguous attempts, not receiver-side exactly-once effects. Prior crash/dedup integration tests address that separate guarantee. No accelerated lease/retry settings are used in this benchmark.

## Observed query bottlenecks and fixes

The first simple 50,000-row single-endpoint fixture did not expose a claim bottleneck (0.039 ms); its [raw plan](before-plan/query-plans.json) is retained. A more discriminating fixture has 100,000 rows: 50,000 target deliveries with ten due now, plus 50,000 earlier due deliveries at another endpoint. The [original skewed plan](before-skewed/query-plans.json) scanned the global due index and filtered 50,000 unrelated rows before returning one target (7.744 ms). The [updated plan](local-2026-10-16-final/query-plans.json) uses `deliveries_endpoint_due_idx`, with tenant/endpoint and stable statement-time due range as index conditions, zero filter removals, and 0.053 ms execution in that observation.

Migration 9 adds the partial tenant/endpoint/due/id index. Due cutoffs use `statement_timestamp()` to permit range bounds; final cycle/lease checks after locks still use the fresh database clock. Admission previously counted all 100,000 live rows in 12.164 ms even when cap rejection was already certain. It now counts at most `limit-additional+1`; the planner probe uses 10,000 as a representative bound, reading 10,000 rows in 1.697 ms. Exact equality, oversize fan-out, freed capacity, idempotent receipts and concurrent admission are covered by integration tests. [ADR 0012](../docs/decisions/0012-measured-query-bounds.md) explains correctness and migration/write-cost tradeoffs.

These are single warmed EXPLAIN ANALYZE observations with buffer counts, not repeated confidence intervals or an end-to-end throughput multiplier. UUID distributions/cache state and concurrent work vary. The global telemetry snapshot still aggregates history (about 19 ms here), a known scaling cost left explicit rather than replaced with an unverified approximate total. Production-size history, many tenants, rollup architecture and index write/storage overhead need further experiments.

## Profiling and security

[CPU](profiles/acceptance.cpu.pprof) and [allocation](profiles/acceptance.heap.pprof) profiles plus [three-repeat benchmark output](profiles/acceptance.txt) isolate event validation/envelope/hash for 1 KiB, 64 KiB and 250 KiB data. These are in-process microbenchmarks, separate from load and with no DB/HTTP. Large data incurs roughly 1.32 MB allocation/op in this implementation; JSON token decoding/copying and envelope allocation are prominent in [allocation output](profiles/alloc-top.txt). Allocation-space totals are cumulative allocated bytes, not live heap or RSS. CPU output is heavily dominated by macOS runtime/system functions, so no application hot-path percentage or production CPU conclusion is inferred. Exact bytes/duplicate-key validation are retained; no parser shortcut was made for an unproven throughput benefit.

[Security review](../docs/security-review.md) records authorization, SSRF, secrets, browser sessions, SQL and telemetry boundaries plus remaining deployment requirements. Official govulncheck v1.8.0 returned zero findings at source/symbol level against its recorded database snapshot. Raw scanner protocol and source-module identity are retained; a clean known-vulnerability scan is not a security certification.

## Limits

This is one shared laptop/VM, two short repetitions, one payload shape and a small tenant count. No hosted measurements, WAN/TLS receiver cost, rate-limit capacity, collector overhead, signed receiver transaction cost, load-balanced sessions, disk-failure/fsync drill, multi-host clock study, long soak, maximum capacity, confidence interval or SLO is claimed. The deliberately slow receiver shows the expected backlog failure mode, and the permanent failure case is reported with all failures visible. Raw results are sufficient to recompute the reported distributions without inventing missing successes. Use measured statements with their workload and duration; do not turn the best short run into an unconditional resume throughput claim.

Primary references: [PostgreSQL multicolumn indexes](https://www.postgresql.org/docs/18/indexes-multicolumn.html), [EXPLAIN](https://www.postgresql.org/docs/18/using-explain.html), [Go profiling](https://go.dev/blog/pprof).

## Final verification and provenance

Formatting/vet, all unit race tests, 41 substantive PostgreSQL/Redis integration race tests plus the gated helper entry, and seven binary smoke targets passed: health, accounts, ingestion, two-worker/signed receiver restart, replay, Redis outage/recovery and console. [Validation logs](validation/integration.txt) and the [final source/binary SHA-256 manifest](local-2026-10-16-final/source-sha256.json) are retained. `go mod verify` passed. Migration 9 applied to development, no generated smoke schemas remained, and project services were stopped with volumes preserved. The optional live observability stack smoke was not rerun for this milestone.

Regenerate the tables without load using `python3 scripts/benchmark-report.py benchmarks/local-2026-10-16-final --output benchmarks/results.md`; this checks raw counts and request percentiles against summaries. Analysis checks run with `python3 -m unittest discover -s scripts -p test_benchmark.py`. The profile target uses fixed files under benchmarks/profiles and overwrites those diagnostic profiles when explicitly rerun; copy them first if retaining a second profile comparison. Load/plan runs always require a new directory.
