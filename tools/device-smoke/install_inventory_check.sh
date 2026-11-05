#!/bin/sh
# Install inventory check: after any pb install, upgrade or rollback, nothing of
# another version may remain on this host. Runs on the host itself, as the
# enrolled user, and only reports (`inventory --check` changes nothing).
#
#   scp tools/device-smoke/install_inventory_check.sh <host>:/tmp/ && ssh <host> sh /tmp/install_inventory_check.sh
# With a sudo password, send it on stdin (never in argv or the environment):
#   printf '%s\n' "$PASS" | ssh <host> PB_SUDO_STDIN=1 sh /tmp/install_inventory_check.sh
#
# Prints the privileged inventory's JSON line. Exit 0 when only the active
# version is present; exit 1 with "remaining" naming each leftover otherwise.
set -eu
uid=$(id -u)
case "$(uname -s)" in
Darwin) pb="/Library/PrivilegedHelperTools/Paperboat/users/u$uid/bin/pb" ;;
*) pb="/usr/local/libexec/paperboat/users/u$uid/bin/pb" ;;
esac
if [ ! -x "$pb" ]; then
	echo "{\"ok\":false,\"error\":\"no Paperboat installation for uid $uid\"}"
	exit 1
fi
# perl alarm bounds the check on both Linux and macOS (no coreutils timeout there).
if [ "${PB_SUDO_STDIN:-}" = 1 ]; then
	perl -e 'alarm 120; exec @ARGV' sudo -S -p '' \
		/usr/bin/env PAPERBOAT_INVOKING_UID="$uid" "$pb" __runtime-service inventory --check
else
	perl -e 'alarm 120; exec @ARGV' sudo -n \
		/usr/bin/env PAPERBOAT_INVOKING_UID="$uid" "$pb" __runtime-service inventory --check
fi
