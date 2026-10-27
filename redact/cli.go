package redact

import (
	"regexp"
	"strings"
)

// Command lines, as text ("mysql -pS3CR3T db") or as argv arrays
// (["mysql", "-pS3CR3T", "db"]).

// cliProgram describes the password options of a program whose short
// options are not self-describing (sshpass -p, mysql -p, curl -u ...).
type cliProgram struct {
	// next are the options whose next argument holds the secret.
	next map[string]bool
	// attached are the option prefixes followed directly by the secret
	// (-pS3CR3T, --user=bob:S3CR3T).
	attached []string
	// sep, if not zero, splits a user name from the secret (bob:S3CR3T,
	// bob%S3CR3T); the user name stays visible and an argument without sep
	// holds no secret.
	sep byte
	// valueOpts are other options that take a separate (non-secret) value.
	valueOpts map[string]bool
	// stopAtArg: the first non-option argument is a command the program
	// runs (sshpass), whose options are not the program's own.
	stopAtArg bool
}

var (
	mysqlClient = &cliProgram{attached: []string{"-p"}}
	sambaClient = &cliProgram{
		next:     map[string]bool{"-U": true, "--user": true},
		attached: []string{"--user=", "-U"}, sep: '%',
	}
	curlClient = &cliProgram{
		next:     map[string]bool{"-u": true, "-U": true, "--user": true, "--proxy-user": true},
		attached: []string{"--user=", "--proxy-user=", "-u", "-U"}, sep: ':',
	}
	// cliPrograms are the programs handled in argv arrays, by base name.
	cliPrograms = map[string]*cliProgram{
		"sshpass": {
			next: map[string]bool{"-p": true}, attached: []string{"-p"},
			valueOpts: map[string]bool{"-f": true, "-d": true, "-P": true}, stopAtArg: true,
		},
		"mysql": mysqlClient, "mysqldump": mysqlClient, "mysqladmin": mysqlClient,
		"mysqlimport": mysqlClient, "mysqlcheck": mysqlClient, "mysqlshow": mysqlClient,
		"mariadb": mysqlClient, "mariadb-dump": mysqlClient, "mariadb-admin": mysqlClient,
		"smbclient": sambaClient, "rpcclient": sambaClient, "smbcacls": sambaClient,
		"smbcquotas": sambaClient, "smbget": sambaClient, "smbtree": sambaClient,
		"curl":     curlClient,
		"ipmitool": {next: map[string]bool{"-P": true}, attached: []string{"-P"}},
	}
)

// optionNegations, as the first word of an option, make it a flag or a
// prompt rather than an option that takes the secret (--no-password,
// --ask-password); optionReferenceWords, as the last word, make its value
// a reference to where the secret is (--password-file /path,
// --password-stdin, --password-env VAR).
var (
	optionNegations = map[string]bool{
		"no": true, "ask": true, "skip": true, "without": true, "disable": true,
	}
	optionReferenceWords = map[string]bool{
		"file": true, "path": true, "stdin": true, "prompt": true, "fd": true,
		"env": true, "command": true, "cmd": true,
	}
)

// optionSecret classifies a long option name (without "--") like
// classifyKey, except that options naming where the secret is
// (--password-file) do not take a secret, nor do negations
// (--no-password, --ask-password) when the value is a separate word, which
// is then the next argument. A negation with an attached value
// (--no-password=X) is still redacted, as are --allow-password and
// --prompt-password, whose value may be the secret.
func optionSecret(name string, attached bool) (bool, metaKind) {
	if segs := segments(name); len(segs) > 1 &&
		((!attached && optionNegations[segs[0]]) || optionReferenceWords[segs[len(segs)-1]]) {
		return false, metaNone
	}
	return classifyKey(name)
}

// looksLikeFlag reports whether the argument after a sensitive long option
// is the next option rather than its value: "-", "--...", or a single
// letter option ("-v"). Any other value starting with '-' ("-S3CR3T") is
// taken as the value.
func looksLikeFlag(arg string) bool {
	return arg == "-" || strings.HasPrefix(arg, "--") ||
		(len(arg) == 2 && arg[0] == '-' && isLetter(arg[1]))
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// secretArg returns arg with its secret redacted: all of it, or the part
// after p.sep. Quotes around the value are kept, as is the '\” idiom
// around it (see quoteIdiom).
func secretArg(arg string, sep byte, meta metaKind) string {
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
		rest, closing := arg[i+1:], ""
		if idiom && strings.HasSuffix(rest, quoteIdiom) {
			rest, closing = rest[:len(rest)-len(quoteIdiom)], quoteIdiom
		} else if q != "" && strings.HasSuffix(rest, q) {
			rest, closing = rest[:len(rest)-1], q
		}
		if !isRedactable(rest) {
			return arg
		}
		return arg[:i+1] + Marker + closing
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

// walkArray redacts the elements of a non-sensitive array. An array of
// strings may be a command line (argv): the value after a sensitive long
// option ("--password", "S3CR3T") is redacted, as are the password options
// of the programs in cliPrograms (sshpass -p, mysql -pS3CR3T, smbclient -U
// user%S3CR3T, curl -u user:S3CR3T, ipmitool -P S3CR3T).
func walkArray(t []interface{}, c ctx) (interface{}, bool) {
	out := make([]interface{}, len(t))
	changed := false
	var prog *cliProgram // the program whose options are being read
	secretNext := false  // the current element is the value of a password option
	skipNext := false    // the current element is the value of another option
	for i, val := range t {
		nv, ch := walk(val, ctx{kerberos: c.kerberos})
		if s, ok := val.(string); ok {
			red := s
			switch {
			case prog != nil && secretNext:
				red = secretArg(s, prog.sep, metaNone)
				secretNext = false
			case prog != nil && skipNext:
				skipNext = false
			case prog != nil && prog.next[s]:
				secretNext = true
			case prog != nil && prog.valueOpts[s]:
				skipNext = true
			case prog != nil && attachedSecret(prog, s) != s:
				red = attachedSecret(prog, s)
			case prog != nil && prog.stopAtArg && !strings.HasPrefix(s, "-"):
				prog = nil
			case i > 0 && sensitiveOption(t[i-1], s):
				red = Marker
			}
			if red != s {
				nv, ch = red, true
			}
			if p := cliPrograms[s[strings.LastIndexByte(s, '/')+1:]]; p != nil {
				prog, secretNext, skipNext = p, false, false
			}
		}
		out[i] = nv
		changed = changed || ch
	}
	return out, changed
}

// attachedSecret redacts arg if it is one of p's options with the secret
// attached (-pS3CR3T, --user=bob:S3CR3T), and returns it unchanged
// otherwise.
func attachedSecret(p *cliProgram, arg string) string {
	for _, a := range p.attached {
		if strings.HasPrefix(arg, a) && len(arg) > len(a) {
			return a + secretArg(arg[len(a):], p.sep, metaNone)
		}
	}
	return arg
}

// sensitiveOption reports whether arg, the argv element after prev, is the
// value of a sensitive long option ("--password", "--s3-secret-access-key").
func sensitiveOption(prev interface{}, arg string) bool {
	p, ok := prev.(string)
	if !ok || !isRedactable(arg) || looksLikeFlag(arg) ||
		!strings.HasPrefix(p, "--") || strings.Contains(p, "=") {
		return false
	}
	sens, meta := optionSecret(p[2:], false)
	return sens && !meta.visible(arg)
}

var (
	// longOption matches a long command-line option followed by a separate
	// value ("--password S3CR3T"); the value starts at the end of the match.
	// "--password=S3CR3T" is a key=value pair and is handled by
	// redactBarePairs.
	longOption = regexp.MustCompile(`--([A-Za-z][A-Za-z0-9_-]{0,63})[ \t]+`)
	// textPrograms match a program name. For sshpass the match extends to
	// the end of its password option and the secret starts at the end of the
	// match. For the others (opt is set) the match is the program name and
	// the white space after it, and every match of opt on the same command
	// is a password option: the secret starts at the end of the match, less
	// back bytes (mysql -pA -pB, curl -u a:A --proxy-user b:B). The command
	// ends at the first '|', ';', '&' or newline that is not inside a quoted
	// string (see commandEnd).
	textPrograms = []textProgram{
		// sshpass -p S3CR3T / -pS3CR3T, after any of its other options. Later
		// options belong to the command sshpass runs.
		{names: []string{"sshpass"}, re: regexp.MustCompile(`\bsshpass(?:(?:[ \t]|\\\r?\n)+(?:-[ev]|-[fdP][ \t]*(?:"(?:[^"\\\n]|\\[^\n])*"|'[^'\n]*'|[^\s"']+)))*(?:[ \t]|\\\r?\n)+-p[ \t]*`)},
		// The attached password of the MySQL and MariaDB clients
		// (mysql -pS3CR3T db). A separate "-p" prompts for the password and
		// is left alone, as is "-p" for other programs, where it often means
		// something else (port, parents).
		programOption([]string{"mysql", "mariadb"},
			`mysql(?:dump|admin|import|check|show)?|mariadb(?:-dump|-admin)?`, `[ \t\n]-p[^\s]`, 1, 0),
		// smbclient -U user%S3CR3T (and the other Samba clients).
		programOption([]string{"smb", "rpcclient"},
			`smbclient|rpcclient|smbcacls|smbcquotas|smbget|smbtree`, `[ \t\n](?:-U[ \t]*|--user(?:=|[ \t]+))`, 0, '%'),
		// curl -u user:S3CR3T, --user, and the proxy forms -U/--proxy-user.
		programOption([]string{"curl"}, `curl`, `[ \t\n](?:-[uU][ \t]*|--(?:proxy-)?user(?:=|[ \t]+))`, 0, ':'),
		// ipmitool -P S3CR3T.
		programOption([]string{"ipmitool"}, `ipmitool`, `[ \t\n]-P[ \t]*`, 0, 0),
	}
)

type textProgram struct {
	names   []string // every match contains one of these
	re, opt *regexp.Regexp
	back    int
	sep     byte
}

// programOption builds the textProgram for the programs matching the
// regexp progs, whose password option matches opt.
func programOption(names []string, progs, opt string, back int, sep byte) textProgram {
	return textProgram{
		names: names,
		re:    regexp.MustCompile(`\b(?:` + progs + `)[ \t]`),
		opt:   regexp.MustCompile(opt),
		back:  back, sep: sep,
	}
}

// quotedEnd returns the end of the quoted string starting at s[i], a single
// or double quote: just after its closing quote, with backslash escapes
// inside double quotes. A string that is not closed runs to the end of the
// line (a '\n' or '\r'), so an unbalanced quote never hides the rest of the
// line from the redaction.
func quotedEnd(s string, i int) int {
	q := s[i]
	k := i + 1
	for k < len(s) && s[k] != q && !isLineEnd(s[k]) {
		if q == '"' && s[k] == '\\' && k+1 < len(s) && !isLineEnd(s[k+1]) {
			k++
		}
		k++
	}
	if k < len(s) && s[k] == q {
		return k + 1
	}
	return k
}

func isLineEnd(c byte) bool { return c == '\n' || c == '\r' }

// escapedQuoteEnd returns the end of the string that starts at the escaped
// quote \q at s[i:], just after the next \q. It is how a quoted string looks
// inside another double-quoted string (sh -c "mysql -e \"a;b\" -pX") or
// inside JSON text. Backslash pairs are skipped, and a string that is not
// closed runs to the end of the line.
func escapedQuoteEnd(s string, i int, q byte) int {
	k := i + 2
	for k < len(s) && !isLineEnd(s[k]) {
		if s[k] == '\\' && k+1 < len(s) && !isLineEnd(s[k+1]) {
			if s[k+1] == q {
				return k + 2
			}
			k++
		}
		k++
	}
	return k
}

// lineContinuation returns the length of the backslash-newline at s[i:]
// (a line continuation), or 0.
func lineContinuation(s string, i int) int {
	if i+1 < len(s) && s[i] == '\\' {
		if s[i+1] == '\n' {
			return 2
		}
		if s[i+1] == '\r' && i+2 < len(s) && s[i+2] == '\n' {
			return 3
		}
	}
	return 0
}

// separatorAt reports whether the unquoted s[i] ends a command: '|', ';'
// and '&', except the '&' of a redirection (2>&1, >&2, <&3, &>file).
func separatorAt(s string, i int) bool {
	switch s[i] {
	case '|', ';':
		return true
	case '&':
		return !(i > 0 && (s[i-1] == '>' || s[i-1] == '<') || i+1 < len(s) && s[i+1] == '>')
	}
	return false
}

// wordRunEnd returns the end of the run of unquoted characters at s[i]: up
// to white space, a quote, or a line continuation, with backslash escapes,
// and also up to an unquoted command separator if stopAtSep.
func wordRunEnd(s string, i int, stopAtSep bool) int {
	j := i
	for j < len(s) {
		switch s[j] {
		case ' ', '\t', '\n', '\r', '"', '\'':
			return j
		case '\\':
			if lineContinuation(s, j) > 0 {
				return j
			}
			if j+1 < len(s) && !isLineEnd(s[j+1]) {
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

// shellWordEnd returns the end of the shell-style word starting at s[i]:
// a quoted string (see quotedEnd), or a run of characters up to white space
// or a quote, with backslash escapes.
func shellWordEnd(s string, i int) int {
	if i < len(s) && (s[i] == '"' || s[i] == '\'') {
		return quotedEnd(s, i)
	}
	return wordRunEnd(s, i, false)
}

// quoteIdiom is how a single quote is written inside a single-quoted
// string: the string is closed, a quote is added and the string is opened
// again, as in sh -c 'mysql -p'\”S 3CR3T'\” db'.
const quoteIdiom = `'\''`

// programWordEnd returns the end of the value of a program's password
// option at s[i]. It is like shellWordEnd, except that an unquoted value
// ends at an unquoted command separator (mysql -pA;mysql -pB), and that a
// value that starts with a quote written inside another quoted string
// ('\”S 3CR3T'\” or \"S 3CR3T\") runs to the matching quote.
func programWordEnd(s string, i int) int {
	switch {
	case strings.HasPrefix(s[i:], quoteIdiom):
		k := i + len(quoteIdiom)
		for k < len(s) && !isLineEnd(s[k]) {
			if strings.HasPrefix(s[k:], quoteIdiom) {
				return k + len(quoteIdiom)
			}
			k++
		}
		return k
	case strings.HasPrefix(s[i:], `\"`):
		return escapedQuoteEnd(s, i, '"')
	case i < len(s) && (s[i] == '"' || s[i] == '\''):
		return quotedEnd(s, i)
	}
	return wordRunEnd(s, i, true)
}

// commandEnd returns the end of the command that continues at s[i]: the
// first '|', ';', '&' (not in a redirection) or line end that is not inside
// a quoted string and not escaped by a backslash. A backslash-newline
// continues the command on the next line. An unbalanced quote runs to the
// end of the line, so the rest of that line belongs to the command. A
// quote escaped by a backslash starts a string that runs to the next
// escaped quote, as in sh -c "mysql -e \"a;b\" -pX", unless it follows a
// closing quote of the same kind ('\”). It reads each byte once.
func commandEnd(s string, i int) int {
	for i < len(s) {
		c := s[i]
		switch {
		case isLineEnd(c) || separatorAt(s, i):
			return i
		case c == '"' || c == '\'':
			i = quotedEnd(s, i)
			continue
		case c == '\\':
			if n := lineContinuation(s, i); n > 0 {
				i += n
				continue
			}
			if i+1 < len(s) && !isLineEnd(s[i+1]) {
				if q := s[i+1]; (q == '"' || q == '\'') && (i == 0 || s[i-1] != q) {
					i = escapedQuoteEnd(s, i, q)
					continue
				}
				i++
			}
		}
		i++
	}
	return len(s)
}

// redactOptions redacts the values of sensitive long options (--password
// S3CR3T) and the password options of the programs in textPrograms.
func redactOptions(s string) string {
	if strings.Contains(s, "--") {
		s = redactLongOptions(s)
	}
	for _, p := range textPrograms {
		for _, n := range p.names {
			if strings.Contains(s, n) {
				s = redactProgramOption(s, p)
				break
			}
		}
	}
	return s
}

func redactLongOptions(s string) string {
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
		if r := secretArg(w, 0, meta); r != w {
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

// redactProgramOption redacts the word at the end of each match of p.re
// (less p.back bytes), the value of a program's password option, and of
// each match of p.opt after it on the same command.
func redactProgramOption(s string, p textProgram) string {
	var b strings.Builder
	pos, written := 0, 0
	redactAt := func(vs int) int {
		end := programWordEnd(s, vs)
		if w := s[vs:end]; w != "" {
			if r := secretArg(w, p.sep, metaNone); r != w {
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
		if p.opt == nil {
			next := pos + m[1]
			pos = max(redactAt(next-p.back), next)
			continue
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
