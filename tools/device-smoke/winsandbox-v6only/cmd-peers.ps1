# Ping and exec from the guest to every machine in $peers (a prefix line in the driver call sets it), with the path state.
foreach ($m in $peers) {
  Log "== pb ping $m"; Log (PB "ping $m --count 3")
  Log "== pb exec $m"; Log (PB "exec $m -- uname -a")
}
