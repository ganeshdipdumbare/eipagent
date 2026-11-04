#!/bin/sh
# Builds the deployable site in docs/public (Vercel serves that folder):
#   index.html         from docs/site.html
#   assets/            fonts, icons, highlight.js and the docs CSS/JS
#   404.html, robots.txt, sitemap.xml, site.webmanifest
#   demo/index.html    the full-stack demo's UI (examples/fullstack/app/ui)
#   demo/recording.js  real runs recorded from the demo's Go backend
#
# Set SKIP_RECORD=1 for a docs-only build; CI records the demo by default.
set -e
cd "$(dirname "$0")"
mkdir -p public/demo
hash=$(find assets -type f | sort | xargs cksum | cksum | awk '{print $1}')
sed \
  -e "s|assets/fonts/fonts.css\"|assets/fonts/fonts.css?v=$hash\"|g" \
  -e "s|assets/site.css\"|assets/site.css?v=$hash\"|g" \
  -e "s|assets/site.js\"|assets/site.js?v=$hash\"|g" \
  -e "s|assets/vendor/highlight/highlight.min.js\"|assets/vendor/highlight/highlight.min.js?v=$hash\"|g" \
  -e "s|assets/vendor/highlight/go.min.js\"|assets/vendor/highlight/go.min.js?v=$hash\"|g" \
  site.html > public/index.html
rm -rf public/assets
cp -R assets public/assets
cp static/404.html static/robots.txt static/sitemap.xml static/site.webmanifest public/
cp ../examples/fullstack/app/ui/index.html public/demo/index.html
if [ "${SKIP_RECORD:-0}" != "1" ]; then
  (cd .. && go run ./examples/fullstack/record docs/public/demo/recording.js >/dev/null)
fi
echo "built docs/public (site, assets, static, demo)"
