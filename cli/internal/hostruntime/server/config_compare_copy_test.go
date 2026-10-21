package server

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
)

// A comparison frame is written after Next returns; Close clears the comparison as soon as the
// stream ends. The frames must not alias the cleared bytes.
func TestConfigComparisonFramesSurviveTheStreamBeingClosed(t *testing.T) {
	local, managed := []byte("machine side"), []byte("repo side")
	stream := &configComparisonStream{
		value:   configsync.ConflictComparison{Local: configsync.ConflictComparisonSide{Content: bytes.Clone(local)}, Managed: configsync.ConflictComparisonSide{Content: bytes.Clone(managed)}},
		release: func() {},
	}
	var data [][]byte
	for {
		frame, err := stream.Next(context.Background())
		var end *StreamEnd
		if errors.As(err, &end) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, frame.Data)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if len(data) != 2 || !bytes.Equal(data[0], local) || !bytes.Equal(data[1], managed) {
		t.Fatalf("frames after Close = %q, want the two sides intact", data)
	}
}
