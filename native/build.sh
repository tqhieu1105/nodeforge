#!/bin/sh
set -eu
cd "$(dirname "$0")"
GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 \
  go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o forge-linux-amd64 .
