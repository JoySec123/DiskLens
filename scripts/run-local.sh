#!/bin/bash
set -eu
ROOT="${1:-$PWD}"
DISKLENS_ROOTS="$ROOT" go run ./cmd/disklens
