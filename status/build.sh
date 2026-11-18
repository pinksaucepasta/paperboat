#!/bin/sh
# Cloudflare Pages build for the status page (root directory: status/, output: dist/). The Worker's address is the only setting:
# STATUS_API_BASE, a Pages environment variable (https://<worker>.<account>.workers.dev). Nothing is built from the Worker's code here.
set -eu
: "${STATUS_API_BASE:?set STATUS_API_BASE in the Pages project's environment variables}"
rm -rf dist && cp -r site dist
printf '// written by build.sh\nexport const API_BASE = "%s"\n' "$STATUS_API_BASE" >dist/config.js
sed -i "s#connect-src 'self'#connect-src 'self' $STATUS_API_BASE#" dist/_headers
