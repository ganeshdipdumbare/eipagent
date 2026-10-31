#!/bin/sh
# Builds the deployable site in docs/public (Vercel serves that folder):
#   index.html         from docs/site.html (also published as the hosted docs)
#   demo/index.html    the full-stack demo's UI (examples/fullstack/app/ui)
#   demo/recording.js  real runs recorded from the demo's Go backend
set -e
cd "$(dirname "$0")"
mkdir -p public/demo
{
  printf '<!doctype html>\n<html lang="en">\n<head>\n<meta charset="utf-8">\n<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">\n</head>\n<body>\n'
  cat site.html
  printf '\n</body>\n</html>\n'
} > public/index.html
cp ../examples/fullstack/app/ui/index.html public/demo/index.html
(cd .. && go run ./examples/fullstack/record docs/public/demo/recording.js >/dev/null)
echo "built docs/public (site, demo, recording)"
