package redact

import (
	"regexp"
	"strings"
)

// Free-text formats beyond key=value pairs: command-line options, YAML
// blocks and name/value entries written as text.

var (
	// longOption matches a long command-line option and a separate value
	// ("--password S3CR3T", "--s3-secret-access-key 'x y'"). The value may
	// not start with '-' (that is the next option). "--password=S3CR3T" is
	// a key=value pair and is handled by redactBarePairs.
	longOption = regexp.MustCompile(`(^|[\s"'\[(,])(--[A-Za-z][A-Za-z0-9_-]{0,63}[ \t]+)("[^"\n]*"|'[^'\n]*'|[^\s"'-][^\s"']*)`)
	// sshpassOption matches the password option of sshpass ("sshpass -p
	// S3CR3T ssh ...", "sshpass -pS3CR3T"), after any of its other options.
	sshpassOption = regexp.MustCompile(`(\bsshpass(?:[ \t]+(?:-[ev]|-[fdP][ \t]*[^\s"']+))*[ \t]+-p[ \t]*)("[^"\n]*"|'[^'\n]*'|[^\s"']+)`)
	// mysqlPassword matches the attached password option of the MySQL and
	// MariaDB clients ("mysql -u root -pS3CR3T db"). A separate "-p" prompts
	// for the password and is left alone, as is "-p" for other programs,
	// where it often means something else (port, parents).
	mysqlPassword = regexp.MustCompile(`(\b(?:mysql(?:dump|admin|import|check|show)?|mariadb(?:-dump|-admin)?)[ \t][^\n|;&]*?[ \t]-p)("[^"\n]*"|'[^'\n]*'|[^\s"']+)`)
	// flowNameValue matches a name/value entry written on one line, as in
	// YAML flow style or a repr: {name: DB_PASS, value: S3CR3T},
	// {'key': 'DB_PASS', 'value': 'S3CR3T'}. Group 1 is the name, group 2
	// everything up to the value, group 3 the value.
	flowNameValue = regexp.MustCompile(`(?:\b|["'])(?:name|key|variable)["']?[ \t]*[:=][ \t]*["']?([A-Za-z_][A-Za-z0-9_.-]{0,127})["']?[ \t]*,[ \t]*(["']?value["']?[ \t]*[:=][ \t]*)("(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'|[^,}\]\s][^,}\]\n]*)`)
	// flowValueName is flowNameValue with the value first. Group 1 is the
	// prefix, group 2 the value, group 3 the rest up to the name, group 4
	// the name.
	flowValueName = regexp.MustCompile(`(\bvalue["']?[ \t]*[:=][ \t]*)("(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'|[^,}\]\s"'][^,}\]\n]*?)([ \t]*,[ \t]*["']?(?:name|key|variable)["']?[ \t]*[:=][ \t]*["']?)([A-Za-z_][A-Za-z0-9_.-]{0,127})`)
	// yamlKeyLine matches a YAML mapping line: indentation, list item
	// dashes, the key and its value (possibly empty).
	yamlKeyLine = regexp.MustCompile(`^([ \t]*)((?:-[ \t]+)*)["']?([A-Za-z_][A-Za-z0-9_.-]{0,127})["']?[ \t]*:(?:[ \t]+(.*?))?[ \t]*$`)
	// yamlBlockIndicator matches the value of a key that opens a block
	// scalar (|, >, |-, >+2 ...), with an optional comment.
	yamlBlockIndicator = regexp.MustCompile(`^[|>][-+0-9]*(?:[ \t]+#.*)?$`)
)

// unquote removes one pair of matching quotes around v.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// redactValueText returns the replacement for a value written as text
// (quoted or not) under a key classified as sensitive/metadata, or v itself
// if it stays visible.
func redactValueText(v string, metadata bool) string {
	inner := unquote(v)
	if !isRedactable(inner) {
		return v
	}
	if inner == v && metadata && isScalarText(v) {
		return v
	}
	if inner != v {
		return v[:1] + Marker + v[:1]
	}
	return Marker
}

// redactOptions redacts the values of sensitive long options, the sshpass
// password option and the attached password of the MySQL clients.
func redactOptions(s string) string {
	if strings.Contains(s, "--") {
		s = longOption.ReplaceAllStringFunc(s, func(m string) string {
			sub := longOption.FindStringSubmatch(m)
			sens, meta := classifyKey(strings.TrimRight(sub[2][2:], " \t"))
			if !sens {
				return m
			}
			return sub[1] + sub[2] + redactValueText(sub[3], meta)
		})
	}
	if strings.Contains(s, "sshpass") {
		s = sshpassOption.ReplaceAllStringFunc(s, func(m string) string {
			sub := sshpassOption.FindStringSubmatch(m)
			return sub[1] + redactValueText(sub[2], false)
		})
	}
	if strings.Contains(s, "mysql") || strings.Contains(s, "mariadb") {
		s = mysqlPassword.ReplaceAllStringFunc(s, func(m string) string {
			sub := mysqlPassword.FindStringSubmatch(m)
			return sub[1] + redactValueText(sub[2], false)
		})
	}
	return s
}

// redactFlowPairs redacts the value of name/value entries written on one
// line whose name is sensitive.
func redactFlowPairs(s string) string {
	if !strings.Contains(s, "value") {
		return s
	}
	s = flowNameValue.ReplaceAllStringFunc(s, func(m string) string {
		sub := flowNameValue.FindStringSubmatchIndex(m)
		sens, meta := classifyKey(m[sub[2]:sub[3]])
		// An unquoted Marker ends before its ']' here.
		if !sens || strings.HasPrefix(Marker, m[sub[6]:]) {
			return m
		}
		val := strings.TrimRight(m[sub[6]:sub[7]], " \t")
		return m[:sub[6]] + redactValueText(val, meta) + m[sub[6]+len(val):]
	})
	return flowValueName.ReplaceAllStringFunc(s, func(m string) string {
		sub := flowValueName.FindStringSubmatch(m)
		sens, meta := classifyKey(sub[4])
		if !sens || strings.HasPrefix(Marker, sub[2]) {
			return m
		}
		return sub[1] + redactValueText(sub[2], meta) + sub[3] + sub[4]
	})
}

// yamlLine is a parsed YAML mapping line.
type yamlLine struct {
	col   int // column of the key
	dash  bool
	key   string
	value string // raw value, possibly empty
	vpos  int    // offset of value in the line
}

func parseYAMLLine(line string) (yamlLine, bool) {
	m := yamlKeyLine.FindStringSubmatchIndex(line)
	if m == nil {
		return yamlLine{}, false
	}
	l := yamlLine{col: m[5] - m[2], dash: m[5] > m[4], key: line[m[6]:m[7]], vpos: len(line)}
	if m[8] >= 0 {
		l.value, l.vpos = line[m[8]:m[9]], m[8]
	}
	return l, true
}

// indentOf returns the column of the first non-blank character of line, or
// -1 for a blank line.
func indentOf(line string) int {
	for i := 0; i < len(line); i++ {
		if line[i] != ' ' && line[i] != '\t' {
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
// Lines are scanned once; a stored value line is revisited at most once.
func redactYAML(s string) string {
	if !strings.Contains(s, "\n") || !strings.Contains(s, ":") {
		return s
	}
	lines := strings.SplitAfter(s, "\n")
	drop := make([]bool, len(lines))
	changed := false

	body := func(i int) string { return strings.TrimRight(lines[i], "\r\n") }
	// blockEnd returns the index after the last content line of a block
	// scalar opened on line i whose key is at column col.
	blockEnd := func(i, col int) int {
		end := i + 1
		for j := i + 1; j < len(lines); j++ {
			ind := indentOf(body(j))
			if ind < 0 {
				continue
			}
			if ind <= col {
				break
			}
			end = j + 1
		}
		return end
	}
	// redactLine redacts the value on line i (parsed as l), dropping the
	// content of a block scalar.
	redactLine := func(i int, l yamlLine, metadata bool, end int) {
		b := body(i)
		var nv string
		if yamlBlockIndicator.MatchString(l.value) {
			if end <= i+1 {
				return // empty block
			}
			for j := i + 1; j < end; j++ {
				drop[j] = true
			}
			nv = Marker
		} else {
			nv = redactValueText(l.value, metadata)
		}
		if nv != l.value {
			lines[i] = b[:l.vpos] + nv + b[l.vpos+len(l.value):] + lines[i][len(b):]
			changed = true
		}
	}

	// items is the stack of open mappings (list items or nested blocks),
	// innermost last, each with the column of its keys.
	type item struct {
		col        int
		named      bool
		sens, meta bool
		valLine    int // a value line seen before the name, or -1
		valParsed  yamlLine
		valEnd     int
	}
	var items []item
	for i := 0; i < len(lines); i++ {
		b := body(i)
		l, ok := parseYAMLLine(b)
		if !ok {
			if ind := indentOf(b); ind >= 0 {
				for len(items) > 0 && items[len(items)-1].col > ind {
					items = items[:len(items)-1]
				}
			}
			continue
		}
		end := i + 1
		if yamlBlockIndicator.MatchString(l.value) {
			end = blockEnd(i, l.col)
		}
		for len(items) > 0 && (items[len(items)-1].col > l.col || (l.dash && items[len(items)-1].col == l.col)) {
			items = items[:len(items)-1]
		}
		if len(items) == 0 || items[len(items)-1].col < l.col {
			items = append(items, item{col: l.col, valLine: -1})
		}
		it := &items[len(items)-1]
		switch l.key {
		case "name", "key", "variable":
			if l.value != "" && end == i+1 {
				it.named = true
				it.sens, it.meta = classifyKey(unquote(l.value))
				if it.sens && it.valLine >= 0 {
					redactLine(it.valLine, it.valParsed, it.meta, it.valEnd)
				}
				it.valLine = -1
			}
		case "value":
			if it.named {
				if it.sens {
					redactLine(i, l, it.meta, end)
				}
			} else {
				it.valLine, it.valParsed, it.valEnd = i, l, end
			}
		}
		if sens, _ := classifyKey(l.key); sens && end > i+1 {
			redactLine(i, l, false, end) // block scalar under a sensitive key
		}
		i = end - 1 // skip block scalar content
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
