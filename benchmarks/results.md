# Per-run local results

Generated from retained raw JSON by `scripts/benchmark-report.py`. These are short local experiments; see [method and limits](README.md). Rates are per actual offer window; acknowledgment latency includes only eventual delivered work and must be read with outcome counts.

| Workload / repetition | Offered events/s | Fan-out | Accepted / offered | Attempts/s | Successes/s | Ingestion p95 ms | Ack p95 ms | Delivered / dead / unfinished | End backlog | Drain s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| healthy / 1 | 10 | 1 | 100 / 100 | 10.00 | 10.00 | 9.30 | 21.14 | 100 / 0 / 0 | 0 | 0.09 |
| healthy / 2 | 10 | 1 | 100 / 100 | 10.00 | 10.00 | 10.53 | 21.99 | 100 / 0 / 0 | 0 | 0.09 |
| healthy / 1 | 50 | 1 | 500 / 500 | 50.00 | 50.00 | 5.80 | 17.58 | 500 / 0 / 0 | 0 | 0.09 |
| healthy / 2 | 50 | 1 | 500 / 500 | 49.99 | 49.99 | 5.70 | 18.02 | 500 / 0 / 0 | 0 | 0.09 |
| healthy / 1 | 100 | 1 | 1000 / 1000 | 99.90 | 99.90 | 5.44 | 14.79 | 1000 / 0 / 0 | 0 | 0.08 |
| healthy / 2 | 100 | 1 | 1000 / 1000 | 99.99 | 99.99 | 5.55 | 14.67 | 1000 / 0 / 0 | 0 | 0.09 |
| slow / 1 | 50 | 1 | 500 / 500 | 37.69 | 36.89 | 7.06 | 3291.77 | 500 / 0 / 0 | 125 | 3.62 |
| slow / 2 | 50 | 1 | 500 / 500 | 37.70 | 36.90 | 7.87 | 3385.60 | 500 / 0 / 0 | 127 | 3.61 |
| permanent / 1 | 10 | 1 | 100 / 100 | 10.00 | 0.00 | 9.40 | — | 0 / 100 / 0 | 0 | 0.10 |
| permanent / 2 | 10 | 1 | 100 / 100 | 10.00 | 0.00 | 9.85 | — | 0 / 100 / 0 | 0 | 0.09 |
| outage / 1 | 10 | 1 | 100 / 100 | 21.09 | 9.60 | 8.81 | 8726.80 | 100 / 0 / 0 | 4 | 2.43 |
| outage / 2 | 10 | 1 | 100 / 100 | 20.10 | 9.70 | 8.93 | 6977.60 | 100 / 0 / 0 | 3 | 7.96 |
| fanout / 1 | 10 | 10 | 100 / 100 | 100.00 | 100.00 | 8.94 | 52.58 | 1000 / 0 / 0 | 0 | 0.09 |
| fanout / 2 | 10 | 10 | 100 / 100 | 99.98 | 99.98 | 9.31 | 53.05 | 1000 / 0 / 0 | 0 | 0.09 |
| crash / 1 | 1 | 1 | 2 / 2 | 1.00 | 0.00 | 10.02 | 32668.10 | 2 / 0 / 0 | 2 | 31.66 |

## Tail latencies and backlog

| Workload / repetition | Ingestion p50 / p95 / p99 ms | Arrival-response p99 ms | Generator lag p99 ms | Queue p95 ms | HTTP p95 ms | Ack p99 ms | Peak sampled backlog |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| healthy / 1 @ 10/s | 7.13 / 9.30 / 10.40 | 14.73 | 5.34 | 16.80 | 1.00 | 24.37 | 0 |
| healthy / 2 @ 10/s | 7.39 / 10.53 / 14.75 | 16.75 | 5.20 | 16.37 | 1.00 | 23.95 | 1 |
| healthy / 1 @ 50/s | 3.70 / 5.80 / 7.30 | 10.07 | 5.02 | 14.82 | 1.00 | 20.57 | 1 |
| healthy / 2 @ 50/s | 3.75 / 5.70 / 8.89 | 12.35 | 5.04 | 15.95 | 1.00 | 22.98 | 1 |
| healthy / 1 @ 100/s | 2.88 / 5.44 / 6.67 | 7.75 | 2.51 | 12.95 | 0.00 | 19.20 | 1 |
| healthy / 2 @ 100/s | 3.03 / 5.55 / 7.01 | 8.26 | 2.52 | 12.40 | 0.00 | 19.08 | 2 |
| slow / 1 @ 50/s | 4.91 / 7.06 / 9.52 | 12.29 | 5.11 | 3084.73 | 206.00 | 3443.43 | 115 |
| slow / 2 @ 50/s | 5.24 / 7.87 / 10.26 | 14.18 | 5.11 | 3175.06 | 206.00 | 3604.89 | 128 |
| permanent / 1 @ 10/s | 7.19 / 9.40 / 14.16 | 19.25 | 6.40 | 16.03 | 1.00 | — | 1 |
| permanent / 2 @ 10/s | 6.91 / 9.85 / 15.83 | 17.01 | 5.18 | 15.46 | 1.00 | — | 0 |
| outage / 1 @ 10/s | 6.12 / 8.81 / 10.20 | 13.28 | 5.19 | 15.46 | 1.00 | 9684.10 | 47 |
| outage / 2 @ 10/s | 6.34 / 8.93 / 10.91 | 14.13 | 5.24 | 17.32 | 1.00 | 14274.69 | 38 |
| fanout / 1 @ 10/s | 6.88 / 8.94 / 10.59 | 13.68 | 5.15 | 51.10 | 0.00 | 60.10 | 10 |
| fanout / 2 @ 10/s | 6.74 / 9.31 / 9.91 | 14.67 | 5.15 | 51.75 | 0.00 | 59.07 | 10 |
| crash / 1 @ 1/s | 6.21 / 10.02 / 10.02 | 10.98 | 4.77 | 18.50 | 1.00 | 32668.10 | 2 |

## Database resource samples

Coarse Docker samples; CPU uses Docker percent semantics (may exceed 100% across cores). Memory includes PostgreSQL caches and is not API/worker RSS. Sampling itself adds load. Aggregate child resource usage includes admin/migration/Docker clients and is retained in summary.json without attributing it to the Go processes.

| Workload / repetition | Database CPU peak % | Database memory samples (first → last) |
| --- | ---: | --- |
| healthy / 1 @ 10/s | 12.47 | 194.5MiB / 7.75GiB → 197.3MiB / 7.75GiB |
| healthy / 2 @ 10/s | 11.78 | 194.1MiB / 7.75GiB → 196.8MiB / 7.75GiB |
| healthy / 1 @ 50/s | 14.94 | 195.9MiB / 7.75GiB → 197MiB / 7.75GiB |
| healthy / 2 @ 50/s | 15.68 | 195.9MiB / 7.75GiB → 200.6MiB / 7.75GiB |
| healthy / 1 @ 100/s | 20.11 | 195.6MiB / 7.75GiB → 198.7MiB / 7.75GiB |
| healthy / 2 @ 100/s | 20.41 | 197.6MiB / 7.75GiB → 202.2MiB / 7.75GiB |
| slow / 1 @ 50/s | 15.57 | 194.8MiB / 7.75GiB → 198.4MiB / 7.75GiB |
| slow / 2 @ 50/s | 17.42 | 195.8MiB / 7.75GiB → 198.8MiB / 7.75GiB |
| permanent / 1 @ 10/s | 16.13 | 194.3MiB / 7.75GiB → 199.6MiB / 7.75GiB |
| permanent / 2 @ 10/s | 13.74 | 195.1MiB / 7.75GiB → 199.6MiB / 7.75GiB |
| outage / 1 @ 10/s | 13.12 | 196.9MiB / 7.75GiB → 199.4MiB / 7.75GiB |
| outage / 2 @ 10/s | 13.39 | 196.9MiB / 7.75GiB → 199.6MiB / 7.75GiB |
| fanout / 1 @ 10/s | 17.52 | 199.3MiB / 7.75GiB → 200.6MiB / 7.75GiB |
| fanout / 2 @ 10/s | 16.72 | 200.3MiB / 7.75GiB → 201.2MiB / 7.75GiB |
| crash / 1 @ 1/s | 16.31 | 182.5MiB / 7.75GiB → 182.7MiB / 7.75GiB |

Crash kill -> all measured deliveries observed terminal: **31.717 s**, with 2 expired leases. Both HTTP requests entered before SIGKILL; exact abandoned attempts and their later attempts remain in raw JSON. This includes recovery scheduling/backoff and polling overhead.

Totals across measured cohorts: `{"accepted": 4802, "dead_letter_deliveries": 200, "delivered": 6402, "nonaccepted_or_dropped": 0, "offered": 4802, "unfinished_deliveries": 0}`. Warmup excluded.

