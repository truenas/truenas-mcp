package redact

import "strings"

// Frozen copies of the command-line scanner as it was before separators
// glued to a secret stopped ending the value (oldProgramWordEnd stops at
// them). The differential tests in cli_amp_test.go compare redactOptions
// with oldRedactOptions. oldSecretArg and oldWordRunEnd are frozen as well,
// so that a change to how a value is cut or redacted is compared too; the
// other helpers are shared. Do not change them.

func oldProgramWordEnd(s string, i int, sep byte) int {
	end := quotedPartEnd(s, i, true)
	if end > i {
		return end
	}
	end = oldWordRunEnd(s, i, true)
	if sep != 0 {
		// the user name, then sep, then a quoted string
		// the word is user<sep>, and then a quoted string
		k := i
		for k < end && s[k] != sep && s[k] != '\\' && s[k] != '\'' && s[k] != '"' {
			k++
		}
		if k < end && s[k] == sep {
			if e := quotedPartEnd(s, k+1, false); e > k+1 {
				return e
			}
		}
	}
	return end
}

func oldRedactOptions(s string) string {
	if strings.Contains(s, "--") {
		s = oldRedactLongOptions(s)
	}
	if strings.Contains(s, "sshpass") {
		s = oldRedactSshpass(s)
	}
	for _, p := range textPrograms {
		for _, n := range p.names {
			if strings.Contains(s, n) {
				s = oldRedactProgramOption(s, p)
				break
			}
		}
	}
	return s
}

func oldRedactLongOptions(s string) string {
	var b strings.Builder
	pos, written := 0, 0
	for pos < len(s) {
		m := longOption.FindStringSubmatchIndex(s[pos:])
		if m == nil {
			break
		}
		start, vs := pos+m[0], pos+m[1]
		name := s[pos+m[2] : pos+m[3]]
		pos = vs
		if start > 0 && !strings.ContainsRune(" \t\r\n\"'[(,", rune(s[start-1])) {
			pos = start + 2 // "a--b": not an option; look for one after it
			continue
		}
		sens, meta := optionSecret(name, false)
		if !sens {
			continue
		}
		end := shellWordEnd(s, vs)
		w := s[vs:end]
		if w == "" || looksLikeFlag(w) {
			continue
		}
		if r := oldSecretArg(w, 0, meta); r != w {
			b.WriteString(s[written:vs])
			b.WriteString(r)
			written = end
		}
		pos = end
	}
	if written == 0 {
		return s
	}
	b.WriteString(s[written:])
	return b.String()
}

func oldRedactSshpass(s string) string {
	var b strings.Builder
	pos, written := 0, 0
	redact := func(vs, end int) {
		if w := s[vs:end]; w != "" {
			if r := oldSecretArg(w, 0, metaNone); r != w {
				b.WriteString(s[written:vs])
				b.WriteString(r)
				written = end
			}
		}
	}
	// word returns the end of the word at i, or i if there is none.
	word := func(i int) (int, bool) {
		i = skipBlanks(s, i)
		if i >= len(s) || isLineEnd(s[i]) || separatorAt(s, i) {
			return i, false
		}
		return oldProgramWordEnd(s, i, 0), true
	}
	cmdFrom, cmdTo := 0, -1 // the command end found last: from where, to where
	for pos < len(s) {
		m := sshpassStart.FindStringIndex(s[pos:])
		if m == nil {
			break
		}
		// The scan starts after the white space or the line continuation.
		i := pos + m[1]
		from := i
		for {
			end, ok := word(i)
			if !ok || end == skipBlanks(s, i) {
				i = max(i, end)
				break
			}
			start := skipBlanks(s, i)
			i = end
			kind, off := sshpassOption(s[start:end])
			if kind == optCommand {
				break
			}
			if kind == optSecretNext || kind == optValueNext {
				// -p'S 3CR3T': the value is attached, and quoted
				if qe := quotedPartEnd(s, end, true); qe > end {
					if kind == optSecretNext {
						redact(end, qe)
					}
					i = qe
					continue
				}
			}
			switch kind {
			case optSecretAttached:
				// -p\"S 3CR3T\": a quoted value written inside another quoted
				// string has white space in it, so the word ended inside it.
				if qe := quotedPartEnd(s, start+off, true); qe > end {
					redact(start+off, qe)
					i = qe
					break
				}
				redact(start+off, end)
			case optSecretNext, optValueNext:
				vs := skipBlanks(s, i)
				if ve, ok := word(i); ok {
					if kind == optSecretNext {
						redact(vs, ve)
					}
					i = ve
				}
			}
		}
		// A word that reads a quote wrongly (the quote idiom after a quoted
		// string) may run to the end of the line, past a separator and the
		// next sshpass: do not go beyond the end of the command. The end is
		// kept while the next sshpass is in the same command, so that each
		// part of the line is read once.
		if i > from {
			if from < cmdFrom || from > cmdTo {
				cmdFrom, cmdTo = from, commandEnd(s, from)
			}
			i = min(i, cmdTo)
		}
		pos = max(i, pos+m[1])
	}
	if written == 0 {
		return s
	}
	b.WriteString(s[written:])
	return b.String()
}

func oldRedactProgramOption(s string, p textProgram) string {
	var b strings.Builder
	pos, written := 0, 0
	redactAt := func(vs int) int {
		end := oldProgramWordEnd(s, vs, p.sep)
		if w := s[vs:end]; w != "" {
			if r := oldSecretArg(w, p.sep, metaNone); r != w {
				b.WriteString(s[written:vs])
				b.WriteString(r)
				written = end
			}
		}
		return end
	}
	for pos < len(s) {
		m := p.re.FindStringIndex(s[pos:])
		if m == nil {
			break
		}
		// The match ends with the white space that starts the options. The
		// command ends at the first '|', ';', '&' or newline outside quotes.
		// An unquoted value that runs past one (-pA;b) is redacted whole,
		// and ends the command.
		pos += m[1] - 1
		cmdEnd := commandEnd(s, pos)
		for {
			om := p.opt.FindStringIndex(s[pos:cmdEnd])
			if om == nil {
				break
			}
			next := pos + om[1]
			pos = max(redactAt(next-p.back), next)
			if pos > cmdEnd {
				break
			}
		}
		pos = max(pos, cmdEnd)
	}
	if written == 0 {
		return s
	}
	b.WriteString(s[written:])
	return b.String()
}

func oldSecretArg(arg string, sep byte, meta metaKind) string {
	if arg == "" {
		return arg
	}
	q := ""
	if arg[0] == '"' || arg[0] == '\'' {
		q = arg[:1]
	}
	idiom := strings.HasPrefix(arg, quoteIdiom)
	if sep != 0 {
		i := strings.IndexByte(arg, sep)
		if i < 0 {
			return arg // a user name alone
		}
		rest, opening, closing := arg[i+1:], "", ""
		switch {
		case idiom && strings.HasSuffix(rest, quoteIdiom):
			rest, closing = rest[:len(rest)-len(quoteIdiom)], quoteIdiom
		case q != "" && strings.HasSuffix(rest, q):
			rest, closing = rest[:len(rest)-1], q
		default:
			// bob:'S3CR3T', or bob:'\''S3CR3T'\'' ('\'' is how sh -c '...'
			// writes a quote), or an escaped quote: the same quote on both ends
			if t := quoteToken(rest); t != "" && len(rest) >= 2*len(t) && strings.HasSuffix(rest, t) {
				opening, closing = t, t
				rest = rest[len(t) : len(rest)-len(t)]
			}
		}
		if !isRedactable(rest) {
			return arg
		}
		return arg[:i+1] + opening + Marker + closing
	}
	if idiom && len(arg) >= 2*len(quoteIdiom) && strings.HasSuffix(arg, quoteIdiom) {
		if inner := arg[len(quoteIdiom) : len(arg)-len(quoteIdiom)]; !isRedactable(inner) || meta.visible(inner) {
			return arg
		}
		return quoteIdiom + Marker + quoteIdiom
	}
	if q != "" && len(arg) >= 2 && strings.HasSuffix(arg, q) {
		if inner := arg[1 : len(arg)-1]; !isRedactable(inner) || meta.visible(inner) {
			return arg
		}
		return q + Marker + q
	}
	if q != "" { // unterminated quote: redact to the end of the line
		return q + Marker
	}
	if !isRedactable(arg) || meta.visible(arg) {
		return arg
	}
	return Marker
}

func oldWordRunEnd(s string, i int, stopAtSep bool) int {
	j := i
	for j < len(s) {
		switch s[j] {
		case ' ', '\t', '\n', '\r', '"', '\'':
			return j
		case '\\':
			if lineContinuation(s, j) > 0 {
				return j
			}
			if stopAtSep && escapedSeparator(s, j) {
				j += 2
			} else if j+1 < len(s) && !isLineEnd(s[j+1]) {
				j++
			}
		default:
			if stopAtSep && separatorAt(s, j) {
				return j
			}
		}
		j++
	}
	return j
}
