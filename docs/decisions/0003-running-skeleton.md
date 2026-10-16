# ADR 0003: Explicit migrations and bounded process lifecycle

Status: accepted. Date: 2026-10-13.

Use Go standard HTTP and slog, pgx pools, and Goose's embedded SQL provider. Verify/pin Go 1.27.2, pgx v5.11.0, Goose v3.29.0, and the official PostgreSQL 18.6-alpine3.24 image tag. Sources: https://go.dev/dl/?mode=json, https://github.com/jackc/pgx/releases/tag/v5.11.0, https://github.com/pressly/goose/releases/tag/v3.29.0, https://raw.githubusercontent.com/docker-library/official-images/master/library/postgres. Source APIs were inspected; runtime evidence is tracked separately.

Run migrations in a separate command, never concurrently from API replicas. Start with the actual tenants foundation table; authentication and provisioning arrive in milestone 2. Migration down is only allowed in development. No custom migration framework is introduced.

Startup requires a bounded PostgreSQL ping; readiness repeats a bounded ping; liveness only confirms the HTTP process runs. Both processes expose health endpoints; worker health defaults to loopback. SIGTERM stops serving and drains HTTP within a bounded grace period, then closes connections. Delivery-specific shutdown extends this foundation in milestone 5.

Keep default environment values development-friendly, while production requires certificate-verified database TLS. OpenTelemetry is planned for operational milestone 8; JSON logging is implemented now. No delivery behavior or telemetry claims are made by the skeleton.
