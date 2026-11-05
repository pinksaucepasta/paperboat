#!/bin/sh
# pt.sh start|stop (on the VM, user pb): a trivial local page on 127.0.0.1:18765 plus a pb preview and an ephemeral
# pb tunnel to it, as user units. start prints the two endpoints (preview first). Port of dadape/feat/pt.sh.
set -u
D=$HOME/gap5/pt
case "$1" in
start)
 mkdir -p $D/www && echo ok > $D/www/index.html
 systemd-run --user -q --unit gap-http -p WorkingDirectory=$D/www python3 -m http.server 18765 --bind 127.0.0.1
 sleep 1
 systemd-run --user -q --unit gap-tunnel -p StandardOutput=file:$D/tunnel.out -p StandardError=file:$D/tunnel.err $HOME/.local/bin/pb tunnel --ephemeral 18765 --json
 systemd-run --user -q --unit gap-preview -p StandardOutput=file:$D/preview.out -p StandardError=file:$D/preview.err $HOME/.local/bin/pb preview 18765 --json
 i=0; while [ $i -lt 40 ]; do
  p=$(grep -ho '"endpoint":"[^"]*"' $D/preview.out 2>/dev/null | head -1 | cut -d'"' -f4); t=$(grep -ho '"endpoint":"[^"]*"' $D/tunnel.out 2>/dev/null | head -1 | cut -d'"' -f4)
  [ -n "$p" ] && [ -n "$t" ] && break; i=$((i+1)); sleep 1; done
 systemctl --user is-active gap-http gap-tunnel gap-preview >&2
 [ -n "$p" ] && [ -n "$t" ] && echo "$p $t" ;;
stop)
 systemctl --user stop gap-tunnel gap-preview gap-http 2>/dev/null; systemctl --user reset-failed gap-tunnel gap-preview gap-http 2>/dev/null
 rm -rf $D; systemctl --user is-active gap-http gap-tunnel gap-preview ;;
esac
