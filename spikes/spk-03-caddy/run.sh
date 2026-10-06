#!/bin/bash
# Runs every SPK-03 scenario and prints the measurements as Markdown.
# From the repo root:
#   docker compose run --rm --no-deps app bash spikes/spk-03-caddy/run.sh
# Gateway/node logs (slog JSON, WARN+) go to /tmp/spike-stderr.log in the container;
# SPIKE_DEBUG=1 prints everything.
set -euo pipefail
export GOPATH=/cache/gopath GOFLAGS=-modcacherw
cd spikes/spk-03-caddy
go vet ./...
go build -o /tmp/spike ./cmd/spike
SPIKE_TMP=/tmp SPIKE_OUT=examples /tmp/spike 2>/tmp/spike-stderr.log
echo
echo "Log lines on stderr by level (WARN+ via slog, lowercase = bypassed slog):"
grep -o '"level":"[A-Za-z]*"' /tmp/spike-stderr.log | sort | uniq -c
