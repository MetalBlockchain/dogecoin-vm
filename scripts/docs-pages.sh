#!/usr/bin/env bash
# Builds the site's developer pages from docs/API.md and docs/RUN-A-NODE.md,
# so the markdown stays the one source:
#
#   scripts/docs-pages.sh        # writes cmd/dogevm/pages/{developers,run-a-node,changelog}.html
#
# Needs pandoc.
set -euo pipefail
cd "$(dirname "$0")/.."

page() { # page SOURCE.md OUT.html
  {
    echo '{{define "head"}}'
    echo '  <script type="module" src="docs.js"></script>'
    echo '{{end}}'
    echo '{{define "main"}}'
    echo '  <main class="page">'
    pandoc --from gfm --to html5 --syntax-highlighting=none "$1" |
      sed -e 's|<table>|<div class="table-wrap"><table class="fees-table">|' \
          -e 's|</table>|</table></div>|' \
          -e 's|href="API.md|href="/developers|g' \
          -e 's|href="RUN-A-NODE.md|href="/run-a-node|g' \
          -e 's|href="https://metaldoge.com/|href="/|g'
    echo '  </main>'
    echo '{{end}}'
  } >"$2"
}

page docs/API.md cmd/dogevm/pages/developers.html
page docs/RUN-A-NODE.md cmd/dogevm/pages/run-a-node.html
page docs/CHANGELOG.md cmd/dogevm/pages/changelog.html
