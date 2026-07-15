package mockmodel

import (
	"io"

	"github.com/milo-os/assistant/agentcore"
)

// sliceStream is an [agentcore.StreamReader] over a fixed slice of parts.
type sliceStream struct {
	parts []agentcore.StreamPart
	i     int
}

func newSliceStream(parts []agentcore.StreamPart) *sliceStream {
	return &sliceStream{parts: parts}
}

func (s *sliceStream) Recv() (agentcore.StreamPart, error) {
	if s.i >= len(s.parts) {
		return agentcore.StreamPart{}, io.EOF
	}
	part := s.parts[s.i]
	s.i++
	return part, nil
}

func (s *sliceStream) Close() error { return nil }
