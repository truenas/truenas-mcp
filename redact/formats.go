package redact

import (
	"regexp"
	"strings"
)

// Free-text formats beyond key=value pairs: name/value entries written on
// one line and YAML blocks. Command lines are in cli.go.

var (
	// flowNameValue matches a name/value entry written on one line, as in
	// YAML flow style or a repr ({name: DB_PASS, value: S3CR3T},
	// {'key': 'DB_PASS', 'value': 'S3CR3T'}), up to the start of the value.
	// Other entries may come between the name and the value
	// ({name: DB_PASS, description: x, value: S3CR3T}), up to 256 bytes and
	// within one pair of braces. Group 1 is the name.
	flowNameValue = regexp.MustCompile(`(?:\b|["'])(?:name|key|variable)["']?[ \t]*[:=][ \t]*["']?([A-Za-z_][A-Za-z0-9_.-]{0,127})["']?[ \t]*,(?:[^{}\[\]\n]{0,256}?,)?[ \t]*["']?value["']?[ \t]*[:=][ \t]*`)
	// flowValueName is the same entry with the value first. Group 1 is the
	// prefix, group 2 the value and any other entries up to the name, group
	// 3 the rest up to the name, group 4 the name.
	flowValueName = regexp.MustCompile(`(\bvalue["']?[ \t]*[:=][ \t]*)([^{}\[\]\n]*?)([ \t]*,[ \t]*["']?(?:name|key|variable)["']?[ \t]*[:=][ \t]*["']?)([A-Za-z_][A-Za-z0-9_.-]{0,127})`)
	// flowNextKey matches the start of the next entry after a ','.
	flowNextKey = regexp.MustCompile(`^[ \t]*["']?[A-Za-z_][A-Za-z0-9_.-]{0,63}["']?[ \t]*[:=]`)
)

// unquote removes one pair of matching quotes around v.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// redactValueText returns the replacement for a value written as text
// (quoted or not) under a key classified as sensitive with metadata kind
// meta, or v itself if it stays visible.
func redactValueText(v string, meta metaKind) string {
	inner := unquote(v)
	if !isRedactable(inner) || meta.visible(inner) {
		return v
	}
	if inner != v {
		return v[:1] + Marker + v[:1]
	}
	return Marker
}

// flowValueEnd returns the end of the flow-style value starting at s[i],
// looking no further than limit: a quoted string (with backslash escapes)
// up to its closing quote, or an unquoted value up to the closing '}' or
// ']', the end of the line or a ',' that starts the next entry ("key:"). A
// ',' inside an unquoted value (S3CR3T,with,commas) does not end it.
func flowValueEnd(s string, i, limit int) int {
	if i < limit && (s[i] == '"' || s[i] == '\'') {
		q := s[i]
		k := i + 1
		for k < limit && s[k] != q && s[k] != '\n' && s[k] != '\r' {
			if s[k] == '\\' && k+1 < limit && s[k+1] != '\n' {
				k++
			}
			k++
		}
		if k < limit && s[k] == q {
			return k + 1
		}
		return k
	}
	k := i
	for ; k < limit; k++ {
		c := s[k]
		if c == '\n' || c == '\r' || c == '}' || c == ']' {
			break
		}
		if c == ',' && flowNextKey.MatchString(s[k+1:min(limit, k+1+72)]) {
			break
		}
	}
	for k > i && (s[k-1] == ' ' || s[k-1] == '\t') {
		k--
	}
	return k
}

// redactFlowPairs redacts the value of name/value entries written on one
// line whose name is sensitive.
func redactFlowPairs(s string) string {
	if !strings.Contains(s, "value") {
		return s
	}
	var b strings.Builder
	pos, written := 0, 0
	for pos < len(s) {
		m := flowNameValue.FindStringSubmatchIndex(s[pos:])
		if m == nil {
			break
		}
		vs := pos + m[1]
		sens, meta := classifyKey(s[pos+m[2] : pos+m[3]])
		if !sens {
			pos += m[3] // a later name may start before this value
			continue
		}
		if strings.HasPrefix(s[vs:], Marker) {
			pos = vs + len(Marker)
			continue
		}
		end := flowValueEnd(s, vs, len(s))
		if r := redactValueText(s[vs:end], meta); r != s[vs:end] {
			b.WriteString(s[written:vs])
			b.WriteString(r)
			written = end
		}
		pos = max(end, vs)
		if pos == vs {
			pos++
		}
	}
	if written > 0 {
		b.WriteString(s[written:])
		s = b.String()
	}
	return flowValueName.ReplaceAllStringFunc(s, func(m string) string {
		sub := flowValueName.FindStringSubmatch(m)
		sens, meta := classifyKey(sub[4])
		v := sub[2]
		if !sens || v == "" || unquote(v) == Marker {
			return m
		}
		end := flowValueEnd(v, 0, len(v))
		val, rest := v[:end], v[end:]
		if strings.Contains(rest, "value") {
			val, rest = v, "" // another value before the name: redact it all
		}
		return sub[1] + redactValueText(val, meta) + rest + sub[3] + sub[4]
	})
}

// yamlLine is a parsed YAML mapping line.
type yamlLine struct {
	col   int // column of the key
	key   string
	value string // raw value, possibly empty
	vpos  int    // offset of value in the line
	dash  bool
}

func isYAMLSpace(c byte) bool { return c == ' ' || c == '\t' }

func isKeyStart(c byte) bool { return isLetter(c) || c == '_' }

func isKeyChar(c byte) bool {
	return isKeyStart(c) || c >= '0' && c <= '9' || c == '.' || c == '-'
}

// parseYAMLLine parses a YAML mapping line: indentation, list item dashes,
// the key (optionally quoted) and its value (possibly empty).
func parseYAMLLine(line string) (yamlLine, bool) {
	i := 0
	for i < len(line) && isYAMLSpace(line[i]) {
		i++
	}
	l := yamlLine{}
	for i+1 < len(line) && line[i] == '-' && isYAMLSpace(line[i+1]) {
		l.dash = true
		for i += 2; i < len(line) && isYAMLSpace(line[i]); i++ {
		}
	}
	l.col = i
	if i < len(line) && (line[i] == '"' || line[i] == '\'') {
		i++
	}
	ks := i
	if i >= len(line) || !isKeyStart(line[i]) {
		return l, false
	}
	for i++; i < len(line) && i-ks < 128 && isKeyChar(line[i]); i++ {
	}
	l.key = line[ks:i]
	if i < len(line) && (line[i] == '"' || line[i] == '\'') {
		i++
	}
	for i < len(line) && isYAMLSpace(line[i]) {
		i++
	}
	if i >= len(line) || line[i] != ':' {
		return l, false
	}
	i++
	if i < len(line) && !isYAMLSpace(line[i]) {
		return l, false // "key:value" is not a mapping
	}
	for i < len(line) && isYAMLSpace(line[i]) {
		i++
	}
	l.value, l.vpos = strings.TrimRight(line[i:], " \t"), i
	return l, true
}

// isBlockIndicator reports whether v opens a block scalar (|, >, |-, >+2
// ...), with an optional comment.
func isBlockIndicator(v string) bool {
	if v == "" || (v[0] != '|' && v[0] != '>') {
		return false
	}
	i := 1
	for i < len(v) && (v[i] == '-' || v[i] == '+' || v[i] >= '0' && v[i] <= '9') {
		i++
	}
	if i == len(v) {
		return true
	}
	if !isYAMLSpace(v[i]) {
		return false
	}
	for i < len(v) && isYAMLSpace(v[i]) {
		i++
	}
	return i < len(v) && v[i] == '#'
}

// indentOf returns the column of the first non-blank character of line, or
// -1 for a blank line.
func indentOf(line string) int {
	for i := 0; i < len(line); i++ {
		if !isYAMLSpace(line[i]) {
			return i
		}
	}
	return -1
}

// redactYAML redacts secrets in multi-line YAML text:
//   - the content of a block scalar (key: |) under a sensitive key, which is
//     replaced with Marker on the key line;
//   - the value of a name/value list entry with a sensitive name, as in
//     Kubernetes container env (- name: DB_PASS / value: S3CR3T), in either
//     order.
//
// The content of other block scalars is scanned like any other lines, so
// YAML embedded in YAML (a ConfigMap's "app.yaml: |") is handled too.
//
// Each line is parsed once; the end of every block scalar is found in one
// more pass, and a stored value line is revisited at most once.
func redactYAML(s string) string {
	if !strings.Contains(s, "\n") || !strings.Contains(s, ":") ||
		(!strings.Contains(s, "value") && !strings.ContainsAny(s, "|>")) {
		return s
	}
	lines := strings.SplitAfter(s, "\n")
	n := len(lines)
	type parsed struct {
		l     yamlLine
		ok    bool
		block bool
		ind   int
		end   int // for a block scalar, the index after its last content line
	}
	p := make([]parsed, n)
	var open []int // block scalars whose content may continue
	last := -1     // the last non-blank line
	for i := range lines {
		b := strings.TrimRight(lines[i], "\r\n")
		p[i].ind = indentOf(b)
		if p[i].ind >= 0 {
			for len(open) > 0 && p[i].ind <= p[open[len(open)-1]].l.col {
				j := open[len(open)-1]
				p[j].end = max(last+1, j+1)
				open = open[:len(open)-1]
			}
			last = i
		}
		p[i].l, p[i].ok = parseYAMLLine(b)
		if p[i].ok && isBlockIndicator(p[i].l.value) {
			p[i].block = true
			open = append(open, i)
		}
	}
	for _, j := range open {
		p[j].end = max(last+1, j+1)
	}

	drop := make([]bool, n)
	changed := false
	// redactLine redacts the value on line i, dropping the content of a
	// block scalar.
	redactLine := func(i int, meta metaKind) {
		l := p[i].l
		b := strings.TrimRight(lines[i], "\r\n")
		var nv string
		if p[i].block {
			if p[i].end <= i+1 {
				return // empty block
			}
			for j := i + 1; j < p[i].end; j++ {
				drop[j] = true
			}
			nv = Marker
		} else {
			nv = redactValueText(l.value, meta)
		}
		if nv != l.value {
			lines[i] = b[:l.vpos] + nv + b[l.vpos+len(l.value):] + lines[i][len(b):]
			changed = true
		}
	}

	// items is the stack of open mappings (list items or nested blocks),
	// innermost last, each with the column of its keys.
	type item struct {
		col     int
		named   bool
		sens    bool
		meta    metaKind
		valLine int // a value line seen before the name, or -1
	}
	var items []item
	for i := 0; i < n; i++ {
		if !p[i].ok {
			if ind := p[i].ind; ind >= 0 {
				for len(items) > 0 && items[len(items)-1].col > ind {
					items = items[:len(items)-1]
				}
			}
			continue
		}
		l := p[i].l
		for len(items) > 0 && (items[len(items)-1].col > l.col || (l.dash && items[len(items)-1].col == l.col)) {
			items = items[:len(items)-1]
		}
		if len(items) == 0 || items[len(items)-1].col < l.col {
			items = append(items, item{col: l.col, valLine: -1})
		}
		it := &items[len(items)-1]
		skip := false
		switch l.key {
		case "name", "key", "variable":
			if l.value != "" && !p[i].block {
				it.named = true
				it.sens, it.meta = classifyKey(unquote(l.value))
				if it.sens && it.valLine >= 0 {
					redactLine(it.valLine, it.meta)
				}
				it.valLine = -1
			}
		case "value":
			if !it.named {
				it.valLine = i
			} else if it.sens {
				redactLine(i, it.meta)
				skip = p[i].block
			}
		}
		if p[i].block && !skip {
			if sens, _ := classifyKey(l.key); sens {
				redactLine(i, metaNone) // block scalar under a sensitive key ("key: |")
				skip = true
			}
		}
		if skip {
			i = p[i].end - 1 // the content was dropped
		}
	}
	if !changed {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	for i, line := range lines {
		if !drop[i] {
			out.WriteString(line)
		}
	}
	return out.String()
}
