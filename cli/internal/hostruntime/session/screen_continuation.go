package session

// screenContinuation retains only an unfinished VT control string or UTF-8
// character. A serialized screen cannot preserve a parser halfway through an
// escape sequence; replaying this suffix after the screen restores that state
// before the next live PTY bytes arrive.
type screenContinuation struct {
	mode      byte
	utf8Left  byte
	stringOSC bool
	overflow  bool
	pending   []byte
}

const maxScreenContinuation = 64 << 10

const (
	screenGround byte = iota
	screenEscape
	screenEscapeIntermediate
	screenCSI
	screenString
	screenStringEscape
)

func (s *screenContinuation) reset() {
	s.mode, s.utf8Left, s.stringOSC, s.overflow = screenGround, 0, false, false
	s.pending = s.pending[:0]
}

func (s *screenContinuation) append(value byte) {
	if s.overflow {
		return
	}
	if len(s.pending) == maxScreenContinuation {
		s.overflow = true
		s.pending = s.pending[:0]
		return
	}
	s.pending = append(s.pending, value)
}

func (s *screenContinuation) feed(data []byte) {
	for _, value := range data {
		s.consume(value)
	}
}

func (s *screenContinuation) consume(value byte) {
	if s.utf8Left != 0 {
		if value >= 0x80 && value <= 0xbf {
			s.append(value)
			s.utf8Left--
			if s.utf8Left == 0 {
				s.reset()
			}
			return
		}
		s.reset()
	}
	switch s.mode {
	case screenGround:
		switch {
		case value == 0x1b:
			s.mode = screenEscape
			s.append(value)
		case value >= 0xc2 && value <= 0xf4:
			s.utf8Left = 1
			if value >= 0xe0 {
				s.utf8Left = 2
			}
			if value >= 0xf0 {
				s.utf8Left = 3
			}
			s.append(value)
		}
	case screenEscape:
		s.append(value)
		switch value {
		case '[':
			s.mode = screenCSI
		case ']':
			s.mode, s.stringOSC = screenString, true
		case 'P', '_', '^', 'X':
			s.mode, s.stringOSC = screenString, false
		default:
			if value >= 0x20 && value <= 0x2f {
				s.mode = screenEscapeIntermediate
			} else {
				s.reset()
			}
		}
	case screenEscapeIntermediate:
		s.append(value)
		if value >= 0x30 || value == 0x18 || value == 0x1a {
			s.reset()
		}
	case screenCSI:
		if value == 0x1b {
			s.reset()
			s.mode = screenEscape
			s.append(value)
			return
		}
		s.append(value)
		if value >= 0x40 && value <= 0x7e || value == 0x18 || value == 0x1a {
			s.reset()
		}
	case screenString:
		s.append(value)
		if s.stringOSC && value == 0x07 || value == 0x18 || value == 0x1a {
			s.reset()
		} else if value == 0x1b {
			s.mode = screenStringEscape
		}
	case screenStringEscape:
		s.append(value)
		if value == '\\' || value == 0x18 || value == 0x1a {
			s.reset()
		} else if value != 0x1b {
			s.mode = screenString
		}
	}
}
