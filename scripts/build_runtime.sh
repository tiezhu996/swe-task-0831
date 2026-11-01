#!/bin/sh
set -eu

skill_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
client_root="$skill_root/runtime/precheck-client"
bin_root="$skill_root/runtime/bin"

mkdir -p "$bin_root"
cd "$client_root"
go build -o "$bin_root/swe-flow" ./cmd/swe-flow
go build -o "$bin_root/swe-submit" ./cmd/swe-submit
