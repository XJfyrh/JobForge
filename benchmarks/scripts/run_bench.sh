#!/usr/bin/env bash
# JobForge Benchmark Runner
# Usage: ./run_bench.sh [jobs] [workers]
#
# Examples:
#   ./run_bench.sh              # Default: 1000 jobs, 4 workers
#   ./run_bench.sh 10000 8      # 10000 jobs, 8 workers

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$(dirname "$SCRIPT_DIR")")"

JOBS="${1:-1000}"
WORKERS="${2:-4}"

echo "=== JobForge Benchmark Suite ==="
echo "Root: $ROOT_DIR"
echo "Jobs: $JOBS, Workers: $WORKERS"
echo

# Check PostgreSQL connection
echo "--- Checking PostgreSQL ---"
if ! pg_isready -h localhost -p 5433 -U jobforge 2>/dev/null; then
    echo "PostgreSQL not ready. Starting with Docker Compose..."
    docker compose -f "$ROOT_DIR/deploy/compose.yaml" up -d postgres
    sleep 5
fi
echo "PostgreSQL ready"
echo

# Run micro benchmarks
echo "--- Micro Benchmarks ---"
cd "$ROOT_DIR/benchmarks/micro"
go test -bench=. -benchmem -benchtime=5s 2>&1 | tee /tmp/jobforge-micro-bench.txt
echo

# Run E2E benchmark
echo "--- E2E Benchmark ---"
cd "$ROOT_DIR/benchmarks/e2e"
go run . -jobs="$JOBS" -workers="$WORKERS" 2>&1 | tee /tmp/jobforge-e2e-bench.txt
echo

echo "=== Benchmark Complete ==="
echo "Results saved to:"
echo "  - /tmp/jobforge-micro-bench.txt"
echo "  - /tmp/jobforge-e2e-bench.txt"
