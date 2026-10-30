#!/bin/sh
set -eu
cd "$(dirname "$0")"
version=${1:-1.0.0}
case "$version" in *[!0-9.]*|'') echo 'Invalid version' >&2; exit 1;; esac
mkdir -p dist
cd service
for driver in wfrac esphome; do
  CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath \
    -ldflags="-s -w -X main.pluginDriver=$driver -X main.pluginVersion=$version" \
    -o "../dist/$driver-linux-armv7" .
done
