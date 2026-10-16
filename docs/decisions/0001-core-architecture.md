# ADR 0001: Go modular monolith and PostgreSQL queue

Status: accepted design. Date: 2026-10-13.

Use Go as the README default; separate API and worker processes in one repository. Share cohesive packages rather than introducing services. Use PostgreSQL as durable storage and the initial queue; use Docker Compose locally and OpenTelemetry for operational visibility. Add Redis only for distributed rate limiting in milestone 7.

One database transaction can commit an event and all delivery rows atomically. A separate broker would introduce a dual-write problem and likely require an outbox. PostgreSQL polling adds database load and needs realistic query-plan/load evidence later; it is an intentional initial tradeoff, not a claim of unlimited scale.

Versions will be checked and pinned at implementation time. No hosted repository identity is assumed; initialize local Git on main without a remote.
