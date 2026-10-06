package config

import "strings"

// keyLines returns the 1-based line of every key defined in a TOML document,
// keyed by its dotted path (components joined with "."). Table headers are
// included. github.com/BurntSushi/toml records key positions but does not
// export them, so this scanner rebuilds them from the source.
//
// It is meant for documents that already parsed successfully: on malformed
// input it stops early and returns what it found.
func keyLines(doc string) map[string]int {
	s := &posScanner{src: doc, line: 1, out: map[string]int{}}
	s.document()

	return s.out
}

type posScanner struct {
	src  string
	pos  int
	line int
	out  map[string]int
}

func (s *posScanner) eof() bool { return s.pos >= len(s.src) }

func (s *posScanner) peek() byte {
	if s.eof() {
		return 0
	}

	return s.src[s.pos]
}

func (s *posScanner) hasPrefix(p string) bool { return strings.HasPrefix(s.src[s.pos:], p) }

func (s *posScanner) advance(n int) {
	for range n {
		if s.eof() {
			return
		}

		if s.src[s.pos] == '\n' {
			s.line++
		}

		s.pos++
	}
}

// skipSpace skips blanks; with newlines it also skips line breaks and comments.
func (s *posScanner) skipSpace(newlines bool) {
	for !s.eof() {
		switch c := s.peek(); {
		case c == ' ' || c == '\t' || c == '\r':
			s.advance(1)
		case c == '\n' && newlines:
			s.advance(1)
		case c == '#' && newlines:
			s.skipComment()
		default:
			return
		}
	}
}

func (s *posScanner) skipComment() {
	for !s.eof() && s.peek() != '\n' {
		s.advance(1)
	}
}

func (s *posScanner) record(path []string, line int) {
	k := strings.Join(path, ".")
	if _, ok := s.out[k]; !ok {
		s.out[k] = line
	}
}

func (s *posScanner) document() {
	var table []string

	for {
		s.skipSpace(true)

		if s.eof() {
			return
		}

		start := s.pos

		switch {
		case s.hasPrefix("[["):
			s.advance(2)
			line := s.line
			table = s.key()
			s.record(table, line)
			s.skipSpace(false)
			s.advance(2) // ]]
		case s.peek() == '[':
			s.advance(1)
			line := s.line
			table = s.key()
			s.record(table, line)
			s.skipSpace(false)
			s.advance(1) // ]
		default:
			s.keyValue(table)
		}

		s.skipSpace(false)

		if s.peek() == '#' {
			s.skipComment()
		}

		if s.pos == start { // malformed input, give up
			return
		}
	}
}

// keyValue parses `key = value` and records the key (and keys of inline tables).
func (s *posScanner) keyValue(table []string) {
	line := s.line
	k := s.key()

	if len(k) == 0 {
		return
	}

	path := append(append([]string(nil), table...), k...)
	s.record(path, line)
	s.skipSpace(false)

	if s.peek() != '=' {
		return
	}

	s.advance(1)
	s.skipSpace(false)
	s.value(path)
}

// key parses a (possibly dotted, possibly quoted) key.
func (s *posScanner) key() []string {
	var parts []string

	for {
		s.skipSpace(false)

		switch s.peek() {
		case '"':
			parts = append(parts, s.basicString())
		case '\'':
			parts = append(parts, s.literalString())
		default:
			start := s.pos
			for !s.eof() && isBareKeyChar(s.peek()) {
				s.advance(1)
			}

			if s.pos == start {
				return parts
			}

			parts = append(parts, s.src[start:s.pos])
		}

		s.skipSpace(false)

		if s.peek() != '.' {
			return parts
		}

		s.advance(1)
	}
}

func isBareKeyChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func (s *posScanner) value(path []string) {
	switch c := s.peek(); {
	case s.hasPrefix(`"""`):
		s.multiline(`"""`, true)
	case s.hasPrefix(`'''`):
		s.multiline(`'''`, false)
	case c == '"':
		s.basicString()
	case c == '\'':
		s.literalString()
	case c == '[':
		s.array(path)
	case c == '{':
		s.inlineTable(path)
	default:
		for !s.eof() && !strings.ContainsRune(",]}#\n \t\r", rune(s.peek())) {
			s.advance(1)
		}
	}
}

func (s *posScanner) array(path []string) {
	s.advance(1) // [

	for !s.eof() {
		s.skipSpace(true)

		switch s.peek() {
		case ']':
			s.advance(1)

			return
		case ',':
			s.advance(1)
		default:
			start := s.pos
			s.value(path)

			if s.pos == start {
				return
			}
		}
	}
}

func (s *posScanner) inlineTable(path []string) {
	s.advance(1) // {

	for !s.eof() {
		s.skipSpace(true)

		switch s.peek() {
		case '}':
			s.advance(1)

			return
		case ',':
			s.advance(1)
		default:
			start := s.pos
			s.keyValue(path)

			if s.pos == start {
				return
			}
		}
	}
}

// basicString consumes a "…" string and returns its raw content (escapes are
// kept as-is, which is enough for key matching of ordinary keys).
func (s *posScanner) basicString() string {
	s.advance(1)

	start := s.pos

	for !s.eof() && s.peek() != '"' && s.peek() != '\n' {
		if s.peek() == '\\' {
			s.advance(1)
		}

		s.advance(1)
	}

	v := s.src[start:s.pos]
	s.advance(1)

	return v
}

func (s *posScanner) literalString() string {
	s.advance(1)

	start := s.pos
	for !s.eof() && s.peek() != '\'' && s.peek() != '\n' {
		s.advance(1)
	}

	v := s.src[start:s.pos]
	s.advance(1)

	return v
}

func (s *posScanner) multiline(delim string, escapes bool) {
	s.advance(3)

	for !s.eof() {
		if escapes && s.peek() == '\\' {
			s.advance(2)

			continue
		}

		if s.hasPrefix(delim) {
			s.advance(3)
			// Up to two extra quotes may close the string ("""a"""" is a" + ").
			for range 2 {
				if s.peek() == delim[0] {
					s.advance(1)
				}
			}

			return
		}

		s.advance(1)
	}
}
