#!/usr/bin/env bash
set -euo pipefail

DSN=${DB_DSN:-postgres://postgres:password@localhost:5432/observability?sslmode=disable}
psql "$DSN" -f db/001_schema.sql
