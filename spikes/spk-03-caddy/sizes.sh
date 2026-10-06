#!/bin/bash
# Binary size and dependency footprint of the gateway options.
# Run inside the dev container from the repo root:
#   docker compose run --rm --no-deps app bash spikes/spk-03-caddy/sizes.sh
set -euo pipefail
export GOPATH=/cache/gopath GOFLAGS=-modcacherw CGO_ENABLED=0
cd spikes/spk-03-caddy
out=$(mktemp -d)
mib() { awk -v s="$1" 'BEGIN{printf "%.1f MiB", s/1048576}'; }
printf '| Build | stripped size | packages linked | modules |\n|---|---|---|---|\n'
for v in baseline minimal nopki standard; do
  go build -trimpath -ldflags='-s -w' -o "$out/$v" ./cmd/size/$v
  pkgs=$(go list -deps ./cmd/size/$v | wc -l)
  mods=$(go list -deps -f '{{with .Module}}{{.Path}}{{end}}' ./cmd/size/$v | sort -u | wc -l)
  printf '| %s | %s | %s | %s |\n' "$v" "$(mib "$(stat -c %s "$out/$v")")" "$pkgs" "$mods"
done
echo
echo "Heaviest modules linked by the minimal build (by package count):"
go list -deps -f '{{with .Module}}{{.Path}}{{end}}' ./cmd/size/minimal | sort | uniq -c | sort -rn | head -12
echo
cd /app
go generate ./... >/dev/null 2>&1 || true
if go build -trimpath -ldflags='-s -w' -o "$out/meshsdr" ./cmd/meshsdr; then
  echo "meshsdr (main module today, no Caddy): $(mib "$(stat -c %s "$out/meshsdr")")"
fi
