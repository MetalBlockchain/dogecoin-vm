#!/usr/bin/env bash
# Renders cmd/dogevm/og/roadmap-diagram.svg to cmd/dogevm/web/roadmap-diagram.png,
# the "How DOGE moves" diagram on /roadmap, at twice its size for sharp
# screens. Needs rsvg-convert (brew install librsvg). If the text changes,
# change the image's alt text in cmd/dogevm/pages/roadmap.html too.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
rsvg-convert --zoom=2 --background-color=white \
  --output="$ROOT/cmd/dogevm/web/roadmap-diagram.png" \
  "$ROOT/cmd/dogevm/og/roadmap-diagram.svg"
echo "wrote cmd/dogevm/web/roadmap-diagram.png"
