# Shared by tools/verify-*.sh (sourced, not run): resolves the module directory from $1.
# Sets $module and $root.
module_setup() {
	[ "$#" -eq 1 ] || { echo "usage: $0 <module>" >&2; exit 64; }
	module=$1
	root=$(CDPATH= cd -- "$(dirname -- "$0")/../$module" 2>/dev/null && pwd) || {
		echo "$0: no such module directory: $module" >&2
		exit 64
	}
	[ -f "$root/go.mod" ] || { echo "$0: $module has no go.mod" >&2; exit 64; }
}

# Fails the check when it changed go.mod or go.sum (go run of a pinned tool can rewrite them).
guard_go_files() {
	snapshot=$(mktemp -d "${TMPDIR:-/tmp}/paperboat-$1.XXXXXX")
	guard_label=$1
	trap guard_cleanup EXIT HUP INT TERM
	cp "$root/go.mod" "$snapshot/go.mod"
	cp "$root/go.sum" "$snapshot/go.sum"
}

guard_cleanup() {
	status=$?
	if ! cmp -s "$root/go.mod" "$snapshot/go.mod" || ! cmp -s "$root/go.sum" "$snapshot/go.sum"; then
		echo "$guard_label changed go.mod or go.sum" >&2
		status=1
	fi
	rm -rf "$snapshot"
	exit "$status"
}
