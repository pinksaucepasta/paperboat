package recording

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Header is the asciicast v2 header line. Standard players ignore the paperboat field.
type Header struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Env       map[string]string `json:"env,omitempty"`
	Paperboat HeaderInfo        `json:"paperboat"`
}

type HeaderInfo struct {
	Recording string `json:"recording"`
	Session   string `json:"session"`
	Trigger   string `json:"trigger"`
	StartedBy string `json:"started_by,omitempty"`
}

// Event codes: output, resize and marker. Typed input is never recorded (it can hold passwords).
const (
	CodeOutput = "o"
	CodeResize = "r"
	CodeMarker = "m"
)

func HeaderLine(header Header) ([]byte, error) {
	header.Version = 2
	line, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}

// EventLine encodes one event as `[seconds, code, data]` and a newline. Invalid UTF-8 in data becomes
// U+FFFD; the host hands whole characters.
func EventLine(elapsed time.Duration, code, data string) []byte {
	var out bytes.Buffer
	out.WriteByte('[')
	out.WriteString(strconv.FormatFloat(elapsed.Seconds(), 'f', 6, 64))
	out.WriteString(`,"`)
	out.WriteString(code)
	out.WriteString(`",`)
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(data) // a string always encodes; Encode adds the newline
	line := out.Bytes()
	line[len(line)-1] = ']'
	return append(line, '\n')
}

func ResizeData(columns, rows uint16) string { return fmt.Sprintf("%dx%d", columns, rows) }

type Event struct {
	Seconds float64
	Code    string
	Data    string
}

func ParseEvent(line []byte) (Event, error) {
	var fields []json.RawMessage
	if json.Unmarshal(line, &fields) != nil || len(fields) != 3 {
		return Event{}, ErrInvalid
	}
	var event Event
	if json.Unmarshal(fields[0], &event.Seconds) != nil || json.Unmarshal(fields[1], &event.Code) != nil || json.Unmarshal(fields[2], &event.Data) != nil || event.Seconds < 0 {
		return Event{}, ErrInvalid
	}
	return event, nil
}
