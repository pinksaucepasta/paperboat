package main

var filesHelpTexts = map[string]helpText{
	"pb inbox": {
		Long: `Manage this machine's Paperboat Inbox, where files sent to it arrive. Three settings and one
queue live here: the local directory received files are written to (path, set, reset), the
account's rule for team file requests (policy), and the team file requests waiting for your
decision (requests, approve, decline). Files stay in the Inbox until you remove them. Sending
is pb send.`,
		Example: `
  pb inbox path
  pb inbox requests
  pb inbox policy automatic`},
	"pb inbox path": {
		Long: `Print the directory on this machine where received files are written. It is a setting of
this machine only and has nothing to do with where your own sends go.`,
		Example: `
  pb inbox path
  pb inbox path --json`},
	"pb inbox set": {
		Long: `Set the directory received files are written to. Give a full path on this computer, written
the way this computer writes paths; ~ at the start means your home directory. A relative path is
refused. The directory is created if it does not exist and must be writable only by you.
Files already delivered stay where they are.`,
		Example: `
  pb inbox set ~/Inbox
  pb inbox set /srv/incoming --json`},
	"pb inbox reset": {
		Long: `Restore the default receiving directory, ~/Paperboat/Inbox, for future files. Files already
delivered stay where they are, and the account's team request policy is unchanged. Use
pb inbox set to choose another directory.`,
		Example: `
  pb inbox reset`},
	"pb inbox requests": {
		Long: `List team file requests, one per line: request ID, sender account, number of files,
status and expiry. A request is a sender asking to deliver an exact set of files; approve or
decline it by its request ID. Filter with --owner, --q and --state; the list stops at --limit
unless you pass --all.`,
		Example: `
  pb inbox requests
  pb inbox requests --state pending --json`},
	"pb inbox approve": {
		Long: `Approve one pending team file request. The sender can then deliver exactly the files in the
request, no others. File names are checked against this computer's rules first, and a request
this system cannot store is refused before it is approved.

--generation is optional: without it the command uses the request's current generation. Pass it
to approve only if the request has not changed since you looked at it.`,
		Example: `
  pb inbox requests
  pb inbox approve request_abc123`},
	"pb inbox decline": {
		Long: `Decline one pending team file request. The sender is told it was declined and cannot
deliver those files. --generation is optional: without it the command uses the request's
current generation.`,
		Example: `
  pb inbox decline request_abc123
  pb inbox decline request_abc123 --json`},
	"pb inbox policy": {
		Long: `Show or set how team file requests are accepted. With no argument it prints the current
setting. manual means you approve each request yourself; automatic accepts them. The setting is
account-wide, not per machine.

Setting the policy also sets the receipt email to match --receipt-email: pass it to send
receipt email, leave it out to turn receipt email off.`,
		Example: `
  pb inbox policy
  pb inbox policy automatic
  pb inbox policy manual --receipt-email`},
	"pb send": {
		Long: `Send files or folders to a machine's Paperboat Inbox. Name the machine with --to. Without
--to the command uses the saved default (pb send destination set) or, inside a terminal
session, that session's destination; it refuses to guess when more than one machine fits.

A folder arrives as a folder of the same name. Links and special files are not sent and are
listed as skipped. The destination must be enrolled, not revoked, and accept incoming files
(pb machine capabilities --file-receive). Sending to this computer copies into its own Inbox.

On a team that approves incoming files, the command waits for the recipient's decision and
exits with an error if it is declined. The command succeeds only once the receiver has verified
and recorded the files. --unlimited lifts the size limit by sending only directly between the
two machines or through your own relay, and waits while neither is available; without it your
account's transfer mode applies.`,
		Example: `
  pb send report.pdf --to db
  pb send ./dist --to laptop
  pb send notes.txt
  pb send big.iso --to db --unlimited`},
	"pb send destination": {
		Long: `Show the saved default machine for pb send, or with --session the destination saved for one
terminal session. --to on a single send overrides it. This setting only chooses where your
sends go; it does not change any Inbox directory.`,
		Example: `
  pb send destination
  pb send destination --session S123 --json`},
	"pb send destination set": {
		Long: `Save a machine, by name or ID, as the default destination for pb send: account-wide, or for
one terminal session with --session. The machine is looked up first, so a wrong name is refused.`,
		Example: `
  pb send destination set laptop
  pb send destination set laptop --session S123`},
	"pb send destination clear": {
		Long: `Clear the saved default send destination, account-wide or for one --session. Transfers
already made and files already received are unaffected; later sends need --to again.`,
		Example: `
  pb send destination clear`},
	"pb send list": {
		Long: `List outgoing transfers to one machine. --on is required: name the machine the
files were sent to. Filter with --q (file name or transfer ID), --state and --session; page
with --limit (default 50) and --offset. Use a listed transfer ID with status or cancel. For
sends to this computer the history is read from the local Inbox.`,
		Example: `
  pb send list --on laptop
  pb send list --on laptop --state failed --json`},
	"pb send status": {
		Long: `Show one outgoing transfer: its delivery progress and receipt. --on is required: name the
machine the files were sent to. Delivered to the receiver is not the same as verified by it;
only the receiver's verified receipt means done.`,
		Example: `
  pb send status transfer_abc --on laptop
  pb send status transfer_abc --on laptop --json`},
	"pb send cancel": {
		Long: `Cancel one outgoing transfer batch by its transfer ID. --on is required: name the machine
the files were sent to. The command prints the batch's resulting state; files that were already
delivered are not deleted.`,
		Example: `
  pb send cancel transfer_abc --on laptop`},
	"pb recordings": {
		Long: `List, replay and export terminal recordings, and start or stop recording a session you are
attached to. Team recordings are made on the recorded machine and stay there, encrypted for the
team; the team's bucket can hold an archived copy. Replay and export are audited by Paperboat
before any data moves, travel over the end-to-end encrypted machine connection, and are
decrypted on your computer with your unlocked ENV vault (pb env unlock).`,
		Example: `
  pb recordings list
  pb recordings play rec_abc123 --speed 2
  pb recordings export rec_abc123 -o session.cast`},
	"pb recordings list": {
		Long: `List recordings you may replay, one per line: ID, machine, start time, length (or
"recording" while it runs), trigger and state. Team admins see all of a team's recordings;
other members see the ones they may replay. --team defaults to the active workspace (in
Personal, your own); --machine limits the list to one machine ID. The list stops at --limit
unless you pass --all.`,
		Example: `
  pb recordings list
  pb recordings list --team acme --machine m_abc123
  pb recordings list --json`},
	"pb recordings play": {
		Long: `Replay one recording in this terminal at its recorded pace. --speed scales the pace and
--idle-limit caps any pause between frames (default 2s); both must be positive. The replay
is audited and needs your ENV vault unlocked; if it is locked the command says to run
pb env unlock. --archive replays the archived copy in the team bucket instead of asking the
machine, which is useful when the machine is offline.`,
		Example: `
  pb recordings play rec_abc123
  pb recordings play rec_abc123 --speed 2 --idle-limit 1s
  pb recordings play rec_abc123 --archive`},
	"pb recordings export": {
		Long: `Write one recording as an asciicast v2 file that standard players read. -o names the file;
without it the recording goes to standard output. The file is created readable by you only and
an existing file is never overwritten; a partial file is removed if the export fails. Like a
replay, the export is audited and needs your unlocked ENV vault. --archive reads the archived
copy in the team bucket instead of asking the machine.`,
		Example: `
  pb recordings export rec_abc123 -o session.cast
  pb recordings export rec_abc123 --archive > session.cast`},
	"pb recordings start": {
		Long: `Start recording the terminal session that a pb connect on this computer is attached to.
Everyone attached sees a REC mark while it records. The session ID is optional; name it when
you are attached to more than one session.

Only admins start recordings (on a personal machine, its owner). The machine needs a recording
key (set up in the console under Terminals > Recordings) and a team also needs the Teams plan;
a refusal says which condition is missing.`,
		Example: `
  pb recordings start
  pb recordings start S123 --json`},
	"pb recordings stop": {
		Long: `Stop recording the terminal session that a pb connect on this computer is attached to. The
session ID is optional; name it when you are attached to more than one session. A recording a
rule started can only be stopped by an admin.`,
		Example: `
  pb recordings stop
  pb recordings stop S123 --json`},
}
