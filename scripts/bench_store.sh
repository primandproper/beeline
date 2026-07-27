#!/usr/bin/env bash
#
# make bench-store — the hot-store benchmark gate (design decision: which
# backend is the blessed distributed-mode default). Seeds 300k estimates into
# each store and measures full-batch BatchGet wall time (p50/p95 reported per
# benchmark) plus a 128-key hot-path read, alongside the in-memory baseline.
# Decision rule: Postgres p95 < 1s on the 300k read → postgres stays the
# single-dependency default; otherwise redis carries the hot store.
#
# The benchmarks provision their own containers (internal/store/postgres/pgtest
# and internal/store/redis/redistest), which is why this script no longer starts
# any. That also makes the measurement fairer than the old shared-server
# preamble did: each backend gets its own container, so the Postgres and Redis
# runs cannot contend for one host's resources. Export
# BEELINE_TEST_POSTGRES_DSN / BEELINE_TEST_REDIS_ADDR to benchmark against
# specific servers instead — which is what you want for a number that is
# supposed to mean something about production hardware.
#
# Needs Docker unless those variables are set.
#
# Overridable via environment: BENCH_TIME (go test -benchtime; iterations,
# default 10x).
set -euo pipefail

BENCH_TIME="${BENCH_TIME:-10x}"

go test -run '^$' -bench 'BenchmarkBatchGet' -benchtime "${BENCH_TIME}" \
  github.com/primandproper/beeline/internal/store/memory \
  github.com/primandproper/beeline/internal/store/postgres \
  github.com/primandproper/beeline/internal/store/redis
