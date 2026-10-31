#!/bin/sh
# Builds docs/public/index.html, the deployable docs site (Vercel serves
# docs/public), from docs/site.html, the page source that is also
# published as the hosted docs.
set -e
cd "$(dirname "$0")"
mkdir -p public
{
  printf '<!doctype html>\n<html lang="en">\n<head>\n<meta charset="utf-8">\n<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">\n</head>\n<body>\n'
  cat site.html
  printf '\n</body>\n</html>\n'
} > public/index.html
echo "wrote docs/public/index.html"
