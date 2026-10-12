package graph

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// ExpandVars replaces $NAME and ${NAME} in s with lookup(NAME), and $$ with $. Any other $ is an error,
// so that a Dockerfile's ${NAME:-default}, which isn't supported, can't silently become empty.
func ExpandVars(s string, lookup func(string) string) (string, error) {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '$')
		if i < 0 {
			return b.String() + s, nil
		}
		b.WriteString(s[:i])
		s = s[i+1:]
		var name string
		switch {
		case strings.HasPrefix(s, "$"):
			b.WriteByte('$')
			s = s[1:]
			continue
		case strings.HasPrefix(s, "{"):
			end := strings.IndexByte(s, '}')
			if end < 0 || !isVarName(s[1:end]) {
				return "", errors.New(`"${" must be followed by a variable's name and "}"; write "$$" for "$"`)
			}
			name, s = s[1:end], s[end+1:]
		default:
			n := 0
			for n < len(s) && isVarName(s[:n+1]) {
				n++
			}
			if n == 0 {
				return "", errors.New(`"$" must be followed by a variable's name or "{"; write "$$" for "$"`)
			}
			name, s = s[:n], s[n:]
		}
		b.WriteString(lookup(name))
	}
}

// isVarName reports whether name is a variable's name that ExpandVars recognizes: a letter or
// underscore, then letters, digits and underscores.
func isVarName(name string) bool {
	for i, r := range name {
		letter := r == '_' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'
		if !letter && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return name != ""
}

// CheckPath checks p, a step's argument param, which may refer to variables: that they are
// written as ExpandVars expects, and that p is absolute, unless it starts with one, which is only
// known once it is expanded.
func CheckPath(param, p string) error {
	if strings.Contains(p, "\x00") {
		return fmt.Errorf("%s %q must not contain a NUL byte", param, p)
	}
	if _, err := ExpandVars(p, func(string) string { return "" }); err != nil {
		return fmt.Errorf("%s %q: %w", param, p, err)
	}
	if !path.IsAbs(p) && !strings.HasPrefix(p, "$") {
		return fmt.Errorf("%s %q must be an absolute path", param, p)
	}
	return nil
}
