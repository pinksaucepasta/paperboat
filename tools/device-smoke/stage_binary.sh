#!/bin/bash
# stage_binary SRC HOST DEST: puts the build SRC at HOST:DEST (mode 755). Source it; HOST "local" is a plain copy.
# The build crosses the link once per host, gzipped and streamed: scp's SFTP stalled to 0.2-0.9 MB/s on the VM link
# (build host to Linux test VM/Linux test VM, 98 MB took 22 and 6 minutes, 2026-11-20) where a pipe held 1.7 MB/s and gzip halves the bytes.
# The host keeps the gunzipped build under /tmp/.pb-stage-<sha256>; every later staging of the same bytes is a
# host-local copy, and caches older than 12 h are swept so a 5 GB VM never fills up.
stage_binary() {
  local src=$1 host=$2 dest=$3 sum cache try
  if [ "$host" = local ]; then cp "$src" "$dest" && chmod 755 "$dest"; return; fi
  sum=$(sha256sum "$src" | cut -c1-24) || return 1
  cache=/tmp/.pb-stage-$sum
  for try in 1 2 3; do
    ssh -n "$host" "find /tmp -maxdepth 1 -name '.pb-stage-*' -mmin +720 -delete 2>/dev/null; [ -s $cache ] && cp $cache $dest" && return 0
    gzip -c "$src" | ssh "$host" "gunzip > $cache.part.$$ && chmod 755 $cache.part.$$ && mv -f $cache.part.$$ $cache || { rm -f $cache.part.$$; exit 1; }"
  done
  return 1
}
