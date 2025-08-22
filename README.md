# XOpsAgent

This example project wires together TimescaleDB, OpenObserve and an OpenTelemetry Collector. A small Go agent receives Alertmanager webhooks, opens a GitHub pull request and optionally checks ArgoCD before closing an incident in TimescaleDB.

## Prerequisites
- Docker and docker-compose
- Go 1.21+

## Getting started

1. Copy `.env.example` to `.env` and fill in the required values for your environment.
2. Start the observability stack:
   ```bash
   docker-compose up -d
   ```
3. Initialize the database schema:
   ```bash
   scripts/load_schema.sh
   ```
4. Build and run the agent:
   ```bash
   go build ./cmd/agent && ./agent
   ```
5. Configure Alertmanager to send webhooks to `http://localhost:8080/webhook`.

The OTEL Collector is configured in `configs/otelcol.yaml` to forward OTLP signals to OpenObserve. Database objects such as the metrics hypertable, a one minute continuous aggregate, a Top‑K view and incident/audit tables are created by `db/001_schema.sql`.

## GitHub PR template
The agent uses `configs/github/pr_template.md` when opening pull requests.

## License
This project is released under the MIT License.
