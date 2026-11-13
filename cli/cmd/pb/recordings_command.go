package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/shared/peerconsumer"
	"io"
	"net"
	nethttp "net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/recording"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
	"github.com/pinksaucepasta/paperboat/shared/teambucket"
	"github.com/spf13/cobra"
)

// recordingsCobraCommand declares `pb recordings`: list a team's terminal recordings and replay or export
// one from its machine over the end-to-end encrypted machine connection.
func recordingsCobraCommand() *cobra.Command {
	group := &cobra.Command{Use: "recordings", Short: "List and replay team terminal recordings", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error { return command.Help() }}
	list := &cobra.Command{Use: "list", Short: "List the recordings you may see", Args: commandArgs(cobra.NoArgs), RunE: actionRun(recordingsList)}
	list.Flags().String("team", "", "team whose recordings to list; defaults to the active team workspace")
	list.Flags().String("machine", "", "only this machine's recordings (machine id)")
	list.Flags().Bool("json", false, "print JSON")
	addListPaging(list)
	play := &cobra.Command{Use: "play <recording-id>", Short: "Replay a recording in this terminal", Args: commandArgs(cobra.ExactArgs(1)), RunE: actionRun(recordingsPlay)}
	play.Flags().Float64("speed", 1, "playback speed")
	play.Flags().Duration("idle-limit", 2*time.Second, "longest pause between frames")
	play.Flags().Bool("archive", false, "replay the copy in the team bucket instead of asking the machine")
	export := &cobra.Command{Use: "export <recording-id>", Short: "Write a recording as an asciicast v2 file", Args: commandArgs(cobra.ExactArgs(1)), RunE: actionRun(recordingsExport)}
	export.Flags().StringP("output", "o", "", "file to write; standard output when omitted")
	export.Flags().Bool("archive", false, "read the copy in the team bucket instead of asking the machine")
	for _, action := range []struct{ use, short, action string }{
		{"start [session-id]", "Start recording the terminal session you are attached to", wire.ActionStart},
		{"stop [session-id]", "Stop the recording of the terminal session you are attached to", wire.ActionStop},
	} {
		action := action
		control := &cobra.Command{Use: action.use, Short: action.short, Args: commandArgs(cobra.MaximumNArgs(1)), RunE: actionRun(func(c *command.Context) error { return recordingsControl(c, action.action) })}
		control.Flags().Bool("json", false, "print JSON")
		group.AddCommand(control)
	}
	group.AddCommand(list, play, export)
	return group
}

func recordingsList(c *command.Context) error {
	client, err := backendClient(c)
	if err != nil {
		return err
	}
	team := strings.TrimSpace(c.String("team"))
	if team == "" {
		team = client.Workspace()
	}
	if team == "" || team == "personal" {
		return localArgumentError("recordings belong to a team: pass --team or switch to a team workspace")
	}
	limit, err := commandListPagingMax(c)
	if err != nil {
		return err
	}
	page, err := client.ListRecordings(c.Context, team, strings.TrimSpace(c.String("machine")), limit, 0)
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, page)
	}
	if len(page.Recordings) == 0 {
		_, err = fmt.Fprintln(c.Writer, "No recordings.")
		return err
	}
	for _, r := range page.Recordings {
		ended := wire.StateRecording
		if r.EndedAt != nil {
			ended = r.EndedAt.Sub(r.StartedAt).Round(time.Second).String()
		}
		if _, err := fmt.Fprintf(c.Writer, "%s  %s  %s  %s  %s  %s\n", r.ID, r.MachineName, r.StartedAt.Local().Format("2006-01-02 15:04 MST"), ended, r.Trigger, r.State); err != nil {
			return err
		}
	}
	writePageTotal(c.Writer, len(page.Recordings), int(page.Total), "recordings")
	return nil
}

// recordingsControl sends record-start or record-stop on the attachment a pb connect on this computer
// holds: the host decides with the team's rules, as for the browser's REC button.
func recordingsControl(c *command.Context, action string) error {
	parent, ok := c.Context.Value(confirmationCommandKey{}).(*cobra.Command)
	if !ok {
		return errors.New("recording control requires CLI command context")
	}
	local, _, err := localDaemonSnapshot(parent, installLocalDaemonService)
	if err != nil {
		return err
	}
	result, err := local.RecordingControl(c.Context, localapi.RecordingControlRequest{SessionID: c.Args().First(), Action: action})
	var remote *localapi.RemoteError
	if errors.As(err, &remote) {
		if message, ok := recordingRefusals[remote.Code]; ok {
			return localArgumentError(message)
		}
		if remote.Code == localapi.RecordingNotAttachedCode || remote.Code == localapi.RecordingAmbiguousCode {
			return localArgumentError(remote.Message + "; recording control acts on a session you are attached to with pb connect")
		}
	}
	if err != nil {
		return err
	}
	if c.Bool("json") {
		return writeCLIJSON(c.Writer, result)
	}
	if result.Recording != nil {
		_, err = fmt.Fprintf(c.Writer, "Recording session %s (%s). Everyone attached sees REC.\n", result.SessionID, result.Recording.ID)
	} else {
		_, err = fmt.Fprintf(c.Writer, "Stopped recording session %s.\n", result.SessionID)
	}
	return err
}

var recordingRefusals = map[string]string{
	wire.ErrorUnavailable:    "recording is not available on this machine: the team needs the Teams plan and a recording key, and the machine must be able to record",
	wire.ErrorForbidden:      "you may not do that: the team lets only admins start recordings, or a rule started this one and only an admin can stop it",
	wire.ErrorActive:         "this session is already being recorded",
	wire.ErrorInactive:       "this session is not being recorded",
	"not_found_or_forbidden": "the session is gone or you are no longer attached to it",
}

func recordingsExport(c *command.Context) (resultErr error) {
	out := c.Writer
	if path := c.String("output"); path != "" {
		file, err := createExportFile(path)
		if err != nil {
			return err
		}
		defer func() {
			if err := file.Close(); resultErr == nil {
				resultErr = err
			}
			if resultErr != nil {
				_ = os.Remove(path)
			}
		}()
		out = file
	}
	return readRecording(c, c.Args().First(), func(text []byte) error { _, err := out.Write(text); return err })
}

// createExportFile creates a new owner-only file: an export never overwrites anything.
func createExportFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func recordingsPlay(c *command.Context) error {
	speed, idle := 1.0, 2*time.Second
	if cmd, ok := c.Context.Value(confirmationCommandKey{}).(*cobra.Command); ok {
		speed, _ = cmd.Flags().GetFloat64("speed")
		idle, _ = cmd.Flags().GetDuration("idle-limit")
	}
	if speed <= 0 || idle <= 0 {
		return localArgumentError("--speed and --idle-limit must be positive")
	}
	player := castPlayer{out: c.Writer, speed: speed, idle: idle.Seconds(), sleep: time.Sleep}
	return readRecording(c, c.Args().First(), player.write)
}

// readRecording asks for a replay ticket (audited by the server), opens the team recording key with this
// account's vault, reads the recording's segments from its machine and hands each one's asciicast lines
// to emit in order.
func readRecording(c *command.Context, id string, emit func([]byte) error) (resultErr error) {
	defer func() { resultErr = namedReplayFailure(resultErr) }()
	if !wire.ValidRecordingID(id) {
		return localArgumentError("not a recording id; see `pb recordings list`")
	}
	client, _, profile, err := e2eeClient(c)
	if err != nil {
		return err
	}
	parent, ok := c.Context.Value(confirmationCommandKey{}).(*cobra.Command)
	if !ok {
		return errors.New("replay requires CLI command context")
	}
	if archive, _ := parent.Flags().GetBool("archive"); archive {
		return readArchive(c, parent, client, id, emit)
	}
	replay, err := client.ReplayRecording(c.Context, id, profile.CLIClientSessionID)
	if err != nil {
		return err
	}
	teamKey, err := openRecordingKey(c, parent, replay)
	if err != nil {
		return err
	}
	defer clear(teamKey)
	// The recording's machine is a team machine: only its team's workspace shows it.
	teamClient, err := client.ForWorkspace(replay.TeamID)
	if err != nil {
		return err
	}
	machine, err := teamClient.GetUserMachine(c.Context, replay.MachineID)
	if err != nil {
		return err
	}
	if machine.InstallationGeneration < 1 {
		return errors.New("the recording's machine is not available to you")
	}
	environmentID, generation := machine.EnvironmentID, uint64(machine.InstallationGeneration)
	local, _, err := localDaemonSnapshot(parent, installLocalDaemonService)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(struct {
		RecordingID string `json:"recording_id"`
	}{id})
	operationID := "op_" + uuid.NewString()
	// The ticket bounds admission only; an admitted replay streams to the end.
	request, err := localapi.NewPeerStreamRequest(replay.MachineID, environmentID, generation, peerconsumer.Recording, operationID, replay.Ticket, time.Now().Add(wire.TicketLifetime), tunnel.MaxRecordingReplayBytes+1<<20, payload)
	if err != nil {
		return err
	}
	request.AccessSessionID = id
	stream, err := local.OpenPeerStream(c.Context, request)
	if err != nil {
		return err
	}
	defer func() {
		if err := stream.Close(); err != nil && !errors.Is(err, net.ErrClosed) && resultErr == nil {
			resultErr = err
		}
	}()
	return decodeRecordingStream(bufio.NewReaderSize(stream, 64<<10), recording.NewReader(teamKey, id, replay.KeyGeneration), id, emit)
}

// openRecordingKey opens the team recording key of a replay answer with this account's unlocked vault.
func openRecordingKey(c *command.Context, parent *cobra.Command, replay wire.Replay) ([]byte, error) {
	vault, err := passwordVaultForCommand(parent)
	if err != nil {
		return nil, err
	}
	chain := make([]recording.ChainLink, len(replay.Chain))
	for i, link := range replay.Chain {
		chain[i] = recording.ChainLink{Generation: link.Generation, SealedPrevious: link.SealedPrevious}
	}
	return vault.RecordingKey(c.Context, replay.TeamID, replay.ViewerKey.Generation, replay.ViewerKey.Sealed, chain, replay.KeyGeneration)
}

// readArchive replays from the team bucket: the server audits the replay and signs one download per archived
// segment; the segments verify and decrypt here exactly as the machine's do.
func readArchive(c *command.Context, parent *cobra.Command, client *api.Client, id string, emit func([]byte) error) error {
	replay, err := client.ReplayRecordingArchive(c.Context, id)
	if err != nil {
		return err
	}
	teamKey, err := openRecordingKey(c, parent, replay)
	if err != nil {
		return err
	}
	defer clear(teamKey)
	reader := recording.NewReader(teamKey, id, replay.KeyGeneration)
	defer reader.Close()
	http := &nethttp.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*nethttp.Request, []*nethttp.Request) error { return nethttp.ErrUseLastResponse }}
	for i, segment := range replay.Segments {
		file, err := downloadSegment(c.Context, http, segment)
		if err != nil {
			return fmt.Errorf("the team bucket did not give segment %d: %w", i, err)
		}
		text, err := reader.Open(file)
		if err != nil {
			return fmt.Errorf("recording segment %d failed verification: %w", i, err)
		}
		if err := emit(text); err != nil {
			return err
		}
	}
	if replay.Final && !reader.Final() {
		return errors.New("the archive is missing the recording's final segment")
	}
	return nil
}

func downloadSegment(ctx context.Context, client *nethttp.Client, download teambucket.Authority) ([]byte, error) {
	request, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodGet, download.URL, nil)
	if err != nil {
		return nil, err
	}
	for name, value := range download.Headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != nethttp.StatusOK {
		return nil, fmt.Errorf("the bucket answered %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, recording.MaxSegmentFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > recording.MaxSegmentFile {
		return nil, errors.New("the segment is larger than any segment can be")
	}
	return data, nil
}

// decodeRecordingStream reads the metadata line and the declared length-prefixed segment files.
func decodeRecordingStream(stream *bufio.Reader, reader *recording.Reader, id string, emit func([]byte) error) error {
	defer reader.Close()
	line, err := stream.ReadSlice('\n')
	if err != nil || len(line) > protocol.MaxStructuredFrame {
		return errors.New("the machine did not describe the recording")
	}
	var metadata tunnel.RecordingReplayMetadata
	if json.Unmarshal(line, &metadata) != nil || metadata.RecordingID != id {
		return errors.New("the machine described another recording")
	}
	var read int64
	for range metadata.Segments {
		var size [4]byte
		if _, err := io.ReadFull(stream, size[:]); err != nil {
			return err
		}
		n := int64(binary.BigEndian.Uint32(size[:]))
		if n > recording.MaxSegmentFile || read+n > metadata.Bytes {
			return errors.New("the recording stream is larger than declared")
		}
		file := make([]byte, n)
		if _, err := io.ReadFull(stream, file); err != nil {
			return err
		}
		read += n
		text, err := reader.Open(file)
		if err != nil {
			return fmt.Errorf("recording segment failed verification: %w", err)
		}
		if err := emit(text); err != nil {
			return err
		}
	}
	if !metadata.Final {
		return nil // still recording: what exists so far
	}
	if !reader.Final() {
		return errors.New("the recording's final segment is missing")
	}
	return nil
}

// castPlayer writes asciicast output events to a terminal at their recorded pace.
type castPlayer struct {
	out         io.Writer
	speed, idle float64
	sleep       func(time.Duration)
	header      bool
	last        float64
	partial     []byte
}

func (p *castPlayer) write(text []byte) error {
	p.partial = append(p.partial, text...)
	for {
		end := bytes.IndexByte(p.partial, '\n')
		if end < 0 {
			return nil
		}
		line := p.partial[:end]
		p.partial = p.partial[end+1:]
		if !p.header { // the header line: the player keeps the viewer's own terminal size
			p.header = true
			continue
		}
		event, err := recording.ParseEvent(line)
		if err != nil {
			return err
		}
		if event.Code != recording.CodeOutput {
			continue
		}
		if wait := min(event.Seconds-p.last, p.idle); wait > 0 {
			p.sleep(time.Duration(wait / p.speed * float64(time.Second)))
		}
		p.last = event.Seconds
		if _, err := io.WriteString(p.out, event.Data); err != nil {
			return err
		}
	}
}

// recordingReplayFailure is a replay that failed at a step this command can name: the text is shown as it is, because the
// generic "could not finish the command" hid why a recording would not play.
type recordingReplayFailure struct{ cause error }

func (e *recordingReplayFailure) Error() string {
	return "The recording could not be read: " + e.cause.Error() + "."
}
func (e *recordingReplayFailure) Unwrap() error { return e.cause }

// namedReplayFailure wraps a failure of the replay itself; typed answers of Paperboat and the local service, usage
// errors and cancellations keep their own messages.
func namedReplayFailure(err error) error {
	var remote *localapi.RemoteError
	var apiFailure *api.APIError
	if err == nil || errors.As(err, &remote) || errors.As(err, &apiFailure) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var usage usageError
	if errors.As(err, &usage) {
		return err
	}
	return &recordingReplayFailure{cause: err}
}
