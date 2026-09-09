package sshcmd

import (
	"fmt"
	"strings"
)

// ParseCommand tokenizes an SSH_ORIGINAL_COMMAND string (R2-Q10). It is
// single-quote aware, like a minimal shell: whitespace separates words and
// single quotes group characters literally (no escape processing inside
// quotes). An unterminated single quote is a hard error: err==nil always
// implies a well-formed argv.
func ParseCommand(s string) ([]string, error) {
	args := []string{}
	var cur strings.Builder
	inQuote := false
	started := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			inQuote = !inQuote
			started = true
		case (c == ' ' || c == '\t' || c == '\n' || c == '\r') && !inQuote:
			if started {
				args = append(args, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated single quote in command %q", s)
	}
	if started {
		args = append(args, cur.String())
	}
	return args, nil
}
