#!/bin/sh
# usage: tools/verify-source-policy.sh <module>
set -eu
. "$(dirname -- "$0")/verify-lib.sh"
module_setup "$@"
cd "$root"
# Without rg every check below would find nothing and pass.
command -v rg >/dev/null || { echo "source-policy: ripgrep (rg) is required" >&2; exit 69; }

metadata_files=$(find . -path './.git' -prune -o -type f \( -name '._*' -o -name '.DS_Store' \) -print)
if [ -n "$metadata_files" ]; then
	printf 'source-metadata: macOS metadata files are not allowed:\n%s\n' "$metadata_files" >&2
	exit 1
fi

status=0
# Every rule runs, so one failing rule does not hide the others.
check() {
	rule=$1
	pattern=$2
	matches=$(rg -n --no-heading --glob '*.go' --glob '!upstream/tailscale/**' --glob '!vendor/**' --glob '!**/*_test.go' "$pattern" . || true)
	[ -z "$matches" ] && return 0
	violations=$(printf '%s\n' "$matches" | while IFS=: read -r file line rest; do
		previous=$((line - 1))
		annotation=$(sed -n "${previous}p" "$file")
		if ! printf '%s\n' "$annotation" | rg -q "^[[:space:]]*//paperboat:allow-source-policy $rule owner=[^[:space:]]+ reason=[^[:space:]]+$"; then
			echo "$rule: unreviewed occurrence at ${file#./}:$line"
		fi
	done)
	[ -z "$violations" ] || { printf '%s\n' "$violations" >&2; return 1; }
}

check sleep '\btime\.Sleep\(' || status=1
check proxy-header 'Header\.(Get|Values)\("(Forwarded|X-Forwarded-For|X-Real-IP|Fly-Client-IP)"' || status=1
check default-http 'http\.Default(Client|Transport)' || status=1
check sensitive-log '(slog\.|log\.(Print|Printf|Println))[^\n]*(candidate|payload|credential|token|private.?key)' || status=1
check metric-label 'WithLabelValues\(' || status=1
check tailscale-import '"tailscale\.com/' || status=1
# The CLI runs on user machines: peer credentials, config dirs and file replacement need an owner.
if [ "$module" = cli ]; then
	check raw-peercred 'Getsockopt(Ucred|Xucred)|SO_PEERCRED|LOCAL_PEERCRED'
	check raw-user-path 'os\.User(Config|Cache)Dir\('
	check atomic-replacement 'os\.CreateTemp\(|os\.Rename\('
fi
exit $status
