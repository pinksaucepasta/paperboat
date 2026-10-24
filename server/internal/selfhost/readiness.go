package selfhost

// A self-host installation is ready only while its node is ready and its
// heartbeat is fresh. Readiness is derived when it is read, never stored, so a
// stopped or uninstalled node stops reporting ready once its heartbeat ages
// out, even before its registry row is deleted. Every listing and lookup uses
// these two definitions.
const (
	nodeLiveSQL          = `n.ready AND n.state='ready' AND n.last_heartbeat_at>now()-interval '15 seconds'`
	installationReadySQL = `coalesce(i.claim_complete AND ` + nodeLiveSQL + ` AND(i.capability='relay' OR n.public_ingress_verified_at IS NOT NULL),false)`
)
