package tunnel

// Carrier output keeps byte/cursor boundaries while the supervisor queues it.
// The local API must never acknowledge a producer cursor ahead of bytes read
// from that queue: a local daemon crash would otherwise skip undelivered data.
type carrierOutputBoundary struct {
	end      uint64
	sequence uint64
	linear   bool
}

type carrierTerminalSource struct {
	*helperTerminalConn
	owner *carrierTerminal
}

func (s *carrierTerminalSource) Read(data []byte) (int, error) {
	n, err := s.helperTerminalConn.Read(data)
	if n > 0 {
		c := s.owner
		c.outputMu.Lock()
		c.produced += uint64(n)
		c.boundaries = append(c.boundaries, carrierOutputBoundary{end: c.produced, sequence: s.readSequence, linear: s.readLinear})
		c.outputMu.Unlock()
	}
	return n, err
}

func (c *carrierTerminal) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	n, err := c.ReconnectingConn.Read(data)
	c.outputMu.Lock()
	c.delivered += uint64(n)
	var sequence uint64
	for len(c.boundaries) > 0 {
		boundary := c.boundaries[0]
		if c.delivered < boundary.end {
			if boundary.linear {
				sequence = boundary.sequence - (boundary.end - c.delivered)
			}
			break
		}
		if boundary.sequence > sequence {
			sequence = boundary.sequence
		}
		c.boundaries = c.boundaries[1:]
	}
	if n == 0 && err != nil {
		sequence = uint64(max(c.sequence.Load(), 0))
	}
	c.outputMu.Unlock()
	if sequence > 0 && c.sequenceSink != nil {
		c.sequenceSink(int(sequence))
	}
	return n, err
}
