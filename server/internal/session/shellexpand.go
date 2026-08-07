package session

import "strings"

// ExpandShellCommand substitutes the spoken arguments into a catalogue entry's
// command template, following the syntax documented on ShellCommand.
//
// Every substituted value is passed through shellQuote *before* it lands in the
// string, so a spoken word can never break out of the shape the catalogue entry
// fixed: operators, quotes, backticks, command substitution and newlines all
// arrive at the remote shell as inert literal text. That quoting is the whole
// point of the seam — the template author decides the command line, the speaker
// only fills in values.
//
//	$1 … $9   the Nth argument, quoted; an absent argument expands to ''
//	$*        every argument not consumed by a $1..$9, each quoted, space-joined
//	$$        a literal dollar sign
//
// A "$" followed by anything else (or trailing at end of template) is left as-is.
func ExpandShellCommand(template string, args []string) string {
	used := referencedArgs(template)
	var b strings.Builder
	for i := 0; i < len(template); i++ {
		c := template[i]
		if c != '$' || i+1 >= len(template) {
			b.WriteByte(c)
			continue
		}
		switch n := template[i+1]; {
		case n == '$':
			b.WriteByte('$')
			i++
		case n >= '1' && n <= '9':
			idx := int(n - '1')
			if idx < len(args) {
				b.WriteString(shellQuote(args[idx]))
			} else {
				b.WriteString("''")
			}
			i++
		case n == '*':
			var rest []string
			for idx, a := range args {
				if !used[idx] {
					rest = append(rest, shellQuote(a))
				}
			}
			b.WriteString(strings.Join(rest, " "))
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// referencedArgs reports which zero-based argument indexes the template names
// explicitly via $1..$9, so $* can expand to only what's left over. A "$$"
// escape consumes both bytes and never counts as a reference.
func referencedArgs(template string) map[int]bool {
	used := map[int]bool{}
	for i := 0; i+1 < len(template); i++ {
		if template[i] != '$' {
			continue
		}
		n := template[i+1]
		switch {
		case n == '$':
			i++
		case n >= '1' && n <= '9':
			used[int(n-'1')] = true
			i++
		}
	}
	return used
}
