# Project progress

## Current milestone

10 — Benchmarks and hardening: complete. Await continuation before milestone 11, deployment/portfolio.

There are 12 milestones numbered 0–11. Milestones 0–10 are complete; milestone 11 remains. Prior browser/backend/observability evidence is preserved in the [milestone 9 record](milestone-9-record.md).

## Completed work

A local-only open-loop Python benchmark harness builds on the existing disposable-schema guard, seeds new synthetic tenants/endpoints through actual APIs, and runs normal compiled API/workers against controlled HTTP receivers. Fifteen experiments cover healthy 10/50/100 events/sec (two repetitions), 200 ms slow receivers, permanent HTTP 400, a temporary HTTP 503 outage, ten-endpoint fan-out (each two repetitions), and a normal-lease SIGKILL/restart probe. Each retains machine-readable offers, delivery/attempt outcomes/timestamps, receiver observations and coarse queue/database resource samples. The report retains exact environment/configuration, generator lag, request and arrival latency, queue/HTTP/ack distributions, attempt/success rates, failures, unfinished counts and drain time. Reproducibility limitations are explicit.

Two ten-second healthy 100-events/sec runs each accepted and delivered all 1,000 events with ingestion p95 5.44–5.55 ms and eventual acknowledgment p95 14.67–14.79 ms. These are local observations, not maximum capacity or an SLO. Slow receivers built 125–127 backlog and then drained; permanent failures dead-lettered all 200 measured deliveries and have no success latency. Outage work recovered after retries. Fan-out accepted 10 events/sec and delivered about 100 deliveries/sec. Crash recovery observed both abandoned deliveries terminal 31.717 seconds after kill with the normal 30-second lease and two lease expirations. Across measured cohorts: 4,802 offered/accepted events, 6,402 delivered, 200 deliberately dead-lettered, zero unfinished deliveries or generator/admission failures. Warmup is excluded from those totals.

The first simple 50,000-row planner fixture was fast. A more discriminating 100,000-row two-endpoint fixture exposed a claim filtering 50,000 other-endpoint rows (7.744 ms). Migration 9 adds a partial tenant/endpoint/due/id index. Stable statement-time due predicates permit index bounds; the updated observed plan reads directly with no filter removals (0.053 ms). Fresh clock-based cycle/lease checks after locks remain unchanged. Admission now counts only up to limit-additional+1 under the existing tenant lock; the planner probe reduced examined rows from 100,000 to 10,000. These single plan observations are not a universal speed multiplier. Global historical queue aggregation remains a measured scaling cost.

Three-repeat Go microbenchmarks and CPU/allocation profiles isolate JSON validation/envelope/hash for 1 KiB, 64 KiB and 250 KiB synthetic data. Profiles are not misrepresented as end-to-end process CPU/RSS. A source/test security review covers tenant isolation, credentials, SSRF, request/resource bounds, console CSRF/session/CSP, telemetry and database boundaries. Official govulncheck v1.8.0 source/symbol scan returned zero findings against its recorded vulnerability database, and module verification passed; this is not a security certification.

See the [report](../benchmarks/README.md), [per-run tables](../benchmarks/results.md), [security review](security-review.md), and [ADR 0012](decisions/0012-measured-query-bounds.md).

## Changed files

scripts/benchmark.py, benchmark-report.py and test_benchmark.py; internal/events/benchmark_test.go; migrations/00009_endpoint_due.sql; internal/repository/postgres/{claim,admission}.go; tests/integration/hardening_test.go; Makefile benchmark/profile/security targets; benchmarks raw environment/experiment/query-plan/profile/scanner/validation artifacts and reports; docs/security-review.md; docs/decisions/0012-measured-query-bounds.md; README.md; docs/{architecture,setup,workers}.md; docs/milestone-9-record.md and this log. No application dependency or existing API contract changed; original specification/private `.env` remain unchanged.

## Executed evidence

On 2026-10-16, all 15 load experiments finished with raw outcomes retained. The generated report rechecks every raw cohort's offer/acceptance/delivery/dead-letter totals and ingestion percentiles against its summary. Crash receiver entry timestamps verify both initial HTTP requests entered before SIGKILL. All deliberate failures appear in the report rather than disappearing from success-only percentiles. CPU/allocation profiles, three-repeat microbenchmark output and before/after EXPLAIN ANALYZE/BUFFERS JSON are retained.

Formatting/vet and all unit race tests passed. Full PostgreSQL/Redis integration race suite passed 42 test entries including the gated helper (41 substantive tests), including the new exact-capacity/oversize-fan-out/freed-capacity test and existing concurrent backlog, tenant lifecycle, crash recovery and fenced lock-wait expiration tests. Two benchmark-analysis checks passed. All modules verified. Scanner exit zero/raw protocol retained; 255 returned OSV database records are not vulnerability findings. Six binaries built. Current smoke and cleanup results are recorded below after final verification.

Initial setup failed because system_profiler was not on the stripped PATH; the helper now uses its absolute path and records unavailable fields. The first complete collection hit Python 3.9 fractional ISO-8601 parsing differences after load; it is explicitly marked incomplete/excluded. The parser was corrected and the entire matrix rerun into a new directory. No failed/incomplete run is treated as a passing result. The first simple planner fixture did not reproduce the cross-endpoint scan problem; both that finding and the later skewed fixture are retained.

## Limits and next work

One shared Apple M4 laptop/Docker VM, two ten-second repetitions, one measured payload shape, four tenants, loopback receiver and disabled rate limiter/traces do not establish production capacity, long-term stability or an SLO. Receiver HMAC/business-transaction performance, TLS/WAN, collector overhead, many-tenant/fairness scaling, calibrated clocks and maximum throughput remain unmeasured. Resource sampling adds load; database CPU/memory are coarse observations, while aggregate child CPU/RSS also include admin/migration/Docker clients. Per-process API/worker CPU/RSS under load is unavailable and not estimated. Parser profiles are separate microbenchmarks, and their runtime-heavy CPU output is not assigned to application hotspots.

Normal transactional index creation can block writers on existing large tables; plan a migration window. Additional index write/storage costs and large-history telemetry rollups require future measurements. Production TLS, dependency ACLs, backups/restores, retention operations, non-root images, CI and hosted synthetic demo remain deployment work. No hosted deployment, remote Git operation, commit, automatic retention job, benchmark above 100 offered healthy events/sec, universal speedup or fabricated resume number is claimed.

Next: milestone 11 — deployment and portfolio. Stop here until continuation.

## Final milestone 10 checks

The final run passed all seven selected binary smoke targets: health/shutdown, accounts, ingestion/idempotency restart, two-worker signed delivery/receiver restart, replay, shared Redis admission/outage recovery, and console workflow. The optional live Prometheus/Grafana/Jaeger smoke was not rerun; milestone 8 retains that evidence and the global telemetry aggregate query is unchanged. Full integration race output, unit/vet output, smoke output, module verification, migration and cleanup logs are retained under benchmarks/validation and benchmarks/security.

Development sender migration 9 applied successfully. The final database query found zero generated smoke schemas. All harness/smoke fixture processes stopped; dev-down removed only this project's PostgreSQL/Redis containers and network, preserving the database volume. No test server remains running. The original supplied specification remains byte-identical, private `.env` remains mode 0600 and unchanged, and its sensitive values are absent from benchmark/source artifacts. New Python syntax, report/document links, raw cohort/crash gates and final source/binary SHA-256 manifest checks passed. Raw report regeneration passed, and both analysis tests passed.


## Post-milestone local startup support

The user's Terminal first rejected GOPROXY, then workspace build-cache access, then reported standard-library packages unavailable under the shared Go toolchain despite dependency downloads succeeding with default caches. Agent-side builds did not prove that Terminal could use the same files. Added local-* Make targets for existing compiled binaries, preserving source build targets. The already-current migration and temporary API/console readiness/page/session checks succeeded with Go deliberately unavailable and invalid Go environment variables. No test tenant/key was provisioned, and fixture processes were stopped without stopping the user's PostgreSQL. This unblocks local use of the shipped executables; a working user Go installation is still needed to rebuild after source edits. Guides now distinguish these workflows without claiming the filesystem denial's underlying cause is established.


## Post-milestone console redesign

At the user's request, replaced the green dashboard and letter/arrow mark with charcoal/blue/slate branding, a custom geometric SVG logo/favicon, a responsive overview/activity chart, current-page status cards, quick filters, publishing shortcuts and readable delivery/event summaries with technical disclosures. Added explicit eight-second live polling with pause control, hidden/dialog/other-view/pending guards and cursor retention. Counts and twelve-hour activity use only returned page records, with scope labels and unknown/unavailable semantics. No fake metrics, new application dependencies, automatic mutation retries, API changes, delivery changes or hosted publishing.

JavaScript syntax, HTML nesting/unique IDs, console Go race tests, formatting/vet, five Node behavior checks and the real compiled console/API/worker smoke passed. Browser verification used the actual existing Demo delivery: real counts/chart, quick filters/empty results, advanced filters, friendly HTTP 204 attempt summary, event payload view, invalid JSON feedback, endpoints navigation, pause/resume and automatic timestamp advancement while visible. At 390px the document width remained 390px with no page-wide horizontal overflow; the desktop viewport was restored. Browser warnings/errors were empty at the recorded final check. Historical UI previews were archived outside the repository during the GitHub cleanup. Final binary built with embedded assets. Source `.env`/private credentials were not changed for this redesign.

The final review preview runs on 8094; user-started 8091 needs a console restart to replace its already-embedded assets. Initial draft preview processes on 8092/8093 could not be stopped: OS process signaling was denied and the tool's Control+C action required a disabled approval category. They were not silently represented as cleaned up. No existing API/worker/receiver was interrupted for this UI task. Milestone 11 remains pending user continuation.

### Funky paper console direction — 2026-10-16

Applied the user's collage-style reference to the functional console: cream grid paper, lime hero, lavender/pink/yellow accents, ink outlines and offset shadows, editorial serif headings, and a new flower relay mark shared by login and favicon. Delivery stickers have gentle motion with reduced-motion support. Existing authentication, publishing, endpoint controls, live updates and current-page metrics are preserved. Browser verified the real local demo delivery plus Events and Endpoints at desktop and 390px widths with no page overflow. Five console UI tests pass; console builds successfully. The draft screenshot was archived during the GitHub cleanup. Local preview uses port 8095; older previews retain their previously compiled assets.

### GitHub source cleanup — 2026-10-16

Replaced the milestone-oriented root README with a project overview, architecture diagram, stack, portable setup, console/API workflow, credentials guide, guarantees, verification commands, documentation index, and remaining deployment work. Archived compiled binaries, draft screenshots, local demo notes, redundant web pointer, caches and placeholder files outside the repository. Preserved private ignored .env and application databases. Removed machine-specific setup instructions and personal workspace paths from engineering records; original specification remains unchanged. Checked publishable files against current private local credentials with no matches, verified README/documentation links, and retained benchmark evidence. Run make build to regenerate binaries before using local-* targets.

Cleanup verification: all Go unit tests, go vet, five console UI tests, and two benchmark-analysis tests passed. Documentation links and exclusion rules validated; original specification byte comparison passed. No Git commit, remote configuration, or push was performed.
