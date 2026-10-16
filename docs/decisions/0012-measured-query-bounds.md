# 0012 — Bound endpoint claim scans and admission counting

Status: accepted for milestone 10.

A 100,000-row synthetic planner fixture exposed a claim scanning 50,000 earlier due rows belonging to another endpoint before finding one target row. The global next-attempt index cannot efficiently separate endpoint identity. Migration 9 adds a partial `(tenant_id, endpoint_id, next_attempt_at, id)` index over queued states. Due predicates use stable `statement_timestamp()` so PostgreSQL can apply an index range condition. This is a conservative per-statement cutoff: work becoming due during that statement can wait until the next poll. Final cycle eligibility after locks/admission and completion lease expiration still use fresh database clock time; lock order and fencing are unchanged.

The same fixture showed admission counting every live row even after rejection was certain. Admission now counts at most `limit - additional + 1` rows under the existing tenant admission lock. More than `limit - additional` implies rejection; an oversized fan-out rejects immediately. No count is exposed as an exact total. Terminal transitions can only free capacity; the previous concurrency argument and idempotency receipt handling remain intact.

We retain the original global due and historical indexes, because other maintenance paths may use them. The new index increases storage/write maintenance; local tests do not establish write amplification at production scale. Goose builds it transactionally with a normal index build; large existing deployments require a maintenance window or a separately designed concurrent index migration. Do not promise zero downtime.

The [report](../../benchmarks/README.md) retains exact plans, seed definitions, measured times and caveats. A single EXPLAIN timing comparison demonstrates plan shape, not a claimed universal throughput multiplier. No counter cache, unsafe count approximation, broader network permission or reduced correctness check was introduced.
