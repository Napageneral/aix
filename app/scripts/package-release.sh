#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NEX_CLI="${ROOT_DIR}/../../../../nex/dist/entry.js"
node "$NEX_CLI" package validate "$ROOT_DIR"
node "$NEX_CLI" package release "$ROOT_DIR"
