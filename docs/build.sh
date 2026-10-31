#!/bin/sh
# Builds docs/index.html (open it in any browser) from docs/site.html, the
# page source that is also published as the hosted docs.
set -e
cd "$(dirname "$0")"
{
  printf '<!doctype html>\n<html lang="en">\n<head>\n<meta charset="utf-8">\n<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">\n</head>\n<body>\n'
  cat site.html
  printf '\n<script src="https://cdn.jsdelivr.net/npm/mermaid@11.4.1/dist/mermaid.min.js"></script>\n'
  printf '<script>mermaid.initialize({ startOnLoad: true, theme: matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "default" });</script>\n'
  printf '</body>\n</html>\n'
} > index.html
echo "wrote docs/index.html"
