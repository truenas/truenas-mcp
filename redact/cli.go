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
	// cluster: the options are short options that may be combined
	// (sshpass -vp S3CR3T), and the first non-option argument is a command
	// the program runs, whose options are not the program's own (sshpass).
	// See sshpassOption.
	cluster bool
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
		"sshpass": {cluster: true},
		"mysql":   mysqlClient, "mysqldump": mysqlClient, "mysqladmin": mysqlClient,
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
// after p.sep. Quotes around the value are kept, as is the quote idiom
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

// quoteToken returns the quote that s starts with: the quote idiom, a quote,
// or an escaped quote (backslashes and a double quote). Otherwise it is "".
func quoteToken(s string) string {
	if strings.HasPrefix(s, quoteIdiom) {
		return quoteIdiom
	}
	if s != "" && (s[0] == '"' || s[0] == '\'') {
		return s[:1]
	}
	if n, q := escapedQuoteRun(s, 0); n > 0 {
		return s[:n] + string(q)
	}
	return ""
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
			case prog != nil && prog.cluster:
				switch kind, off := sshpassOption(s); kind {
				case optSecretNext:
					secretNext = true
				case optValueNext:
					skipNext = true
				case optSecretAttached:
					red = s[:off] + secretArg(s[off:], 0, metaNone)
				case optCommand:
					prog = nil
				}
			case prog != nil && prog.next[s]:
				secretNext = true
			case prog != nil && attachedSecret(prog, s) != s:
				red = attachedSecret(prog, s)
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

// sshpass takes the options -e, -v, -h and -V without a value, -f, -d and -P
// with one, and -p with the password. Short options may be combined
// (-vp S3CR3T, -ep S3CR3T) and a value may be attached (-pS3CR3T, -vf/file).
// The first argument that is not an option is the command it runs, and the
// options after it are that command's own.
const (
	sshpassFlags  = "evhV"
	sshpassValues = "fdP"
)

type optKind int

const (
	optCommand        optKind = iota // not an option: the command sshpass runs
	optFlags                         // options without a value
	optValueNext                     // an option whose (non-secret) value is the next argument
	optValueAttached                 // an option with its (non-secret) value attached
	optSecretNext                    // -p, the password is the next argument
	optSecretAttached                // -p with the password attached, at arg[off:]
)

// sshpassOption classifies an argument of sshpass.
func sshpassOption(arg string) (kind optKind, off int) {
	if len(arg) < 2 || arg[0] != '-' || arg[1] == '-' {
		return optCommand, 0 // "-" and "--" end the options, too
	}
	for k := 1; k < len(arg); k++ {
		switch c := arg[k]; {
		case c == 'p':
			if k+1 < len(arg) {
				return optSecretAttached, k + 1
			}
			return optSecretNext, 0
		case strings.IndexByte(sshpassValues, c) >= 0:
			if k+1 < len(arg) {
				return optValueAttached, 0
			}
			return optValueNext, 0
		case strings.IndexByte(sshpassFlags, c) >= 0 || isLetter(c):
			// a flag, or an option sshpass does not have: no value
		default:
			return optFlags, 0
		}
	}
	return optFlags, 0
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
	// textPrograms match a program name and the white space after it, and
	// every match of opt on the same command is a password option: the
	// secret starts at the end of the match, less back bytes (mysql -pA
	// -pB, curl -u a:A --proxy-user b:B). The command ends at the first '|',
	// ';', '&' or newline that is not inside a quoted string (see
	// commandEnd). sshpass is read by redactSshpass.
	textPrograms = []textProgram{
		// The attached password of the MySQL and MariaDB clients
		// (mysql -pS3CR3T db). A separate "-p" prompts for the password and
		// is left alone, as is "-p" for other programs, where it often means
		// something else (port, parents).
		programOption([]string{"mysql", "mariadb"},
			`mysql(?:dump|admin|import|check|show)?|mariadb(?:-dump|-admin)?`, `[ \t\r\n]-p[^\s]`, 1, 0),
		// smbclient -U user%S3CR3T (and the other Samba clients).
		programOption([]string{"smb", "rpcclient"},
			`smbclient|rpcclient|smbcacls|smbcquotas|smbget|smbtree`, `[ \t\r\n](?:-U[ \t\r]*|--user(?:=|[ \t\r]+))`, 0, '%'),
		// curl -u user:S3CR3T, --user, and the proxy forms -U/--proxy-user.
		programOption([]string{"curl"}, `curl`, `[ \t\r\n](?:-[uU][ \t\r]*|--(?:proxy-)?user(?:=|[ \t\r]+))`, 0, ':'),
		// ipmitool -P S3CR3T.
		programOption([]string{"ipmitool"}, `ipmitool`, `[ \t\r\n]-P[ \t\r]*`, 0, 0),
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
		re:    regexp.MustCompile(`\b(?:` + progs + `)[ \t\r]`),
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

// escapedSeparator reports whether s[i:] is a doubled backslash and a
// separator character (\\|, \\;, \\&): the text may be inside another string
// (JSON text, sh -c "..."), where it is an escaped separator, not one. See
// commandEnd.
func escapedSeparator(s string, i int) bool {
	return i+2 < len(s) && s[i] == '\\' && s[i+1] == '\\' && (s[i+2] == '|' || s[i+2] == ';' || s[i+2] == '&')
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
// again. Inside sh -c '...', the password in mysql -p'S 3CR3T' is written
//
//	sh -c 'mysql -p'\''S 3CR3T'\'' db'
const quoteIdiom = `'\''`

// escapedQuoteRun returns the number of backslashes and the quote at s[i:]
// if it starts with an odd number of backslashes followed by a quote: an
// escaped quote, as in \" or, one level of escaping deeper, \\\".
func escapedQuoteRun(s string, i int) (n int, q byte) {
	j := i
	for j < len(s) && s[j] == '\\' {
		j++
	}
	if n = j - i; n%2 == 1 && j < len(s) && (s[j] == '"' || s[j] == '\'') {
		return n, s[j]
	}
	return 0, 0
}

// escapedRunEnd returns the end of the string that starts at the escaped
// quote at s[i:], which is n backslashes and the quote q: just after the next
// run of exactly n backslashes and q. A string that is not closed runs to
// the end of the line, and closed is false.
func escapedRunEnd(s string, i, n int, q byte) (end int, closed bool) {
	k := i + n + 1
	for k < len(s) && !isLineEnd(s[k]) {
		if s[k] != '\\' {
			k++
			continue
		}
		j := k
		for j < len(s) && s[j] == '\\' {
			j++
		}
		if j < len(s) && s[j] == q && j-k == n {
			return j + 1, true
		}
		k = j
	}
	return k, false
}

// programWordEnd returns the end of the value of a program's password
// option at s[i]. It is like shellWordEnd, except that an unquoted value
// ends at an unquoted command separator (mysql -pA;mysql -pB), and that a
// value that starts with a quote written inside another quoted string (the
// quote idiom or an escaped quote, \"S 3CR3T\") runs to the matching quote.
// If sep is not zero, a value like bob:'S 3CR3T' that has a quote right
// after the user name and sep is read to the end of that quoted string.
func programWordEnd(s string, i int, sep byte) int {
	end := quotedPartEnd(s, i, true)
	if end > i {
		return end
	}
	end = wordRunEnd(s, i, true)
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

// quotedPartEnd returns the end of the quoted string at s[i:], or i if there
// is none: a quoted string, the quote idiom, or a string that starts with an
// escaped quote. An escaped quote that is not closed on the line is a quoted
// string only if lone is set.
func quotedPartEnd(s string, i int, lone bool) int {
	switch {
	case i >= len(s):
	case strings.HasPrefix(s[i:], quoteIdiom):
		return quoteIdiomEnd(s, i)
	case s[i] == '"' || s[i] == '\'':
		return quotedEnd(s, i)
	case s[i] == '\\':
		if n, q := escapedQuoteRun(s, i); n > 0 {
			if end, closed := escapedRunEnd(s, i, n, q); closed || lone {
				return end
			}
		}
	}
	return i
}

// quoteIdiomEnd returns the end of the string that starts with the quote
// idiom at s[i:], just after the next one, or the end of the line.
func quoteIdiomEnd(s string, i int) int {
	k := i + len(quoteIdiom)
	for k < len(s) && !isLineEnd(s[k]) {
		if strings.HasPrefix(s[k:], quoteIdiom) {
			return k + len(quoteIdiom)
		}
		k++
	}
	return k
}

// commandEnd returns the end of the command that continues at s[i]: the
// first '|', ';', '&' (not in a redirection) or line end that is not inside
// a quoted string and not escaped by a backslash. A backslash-newline
// continues the command on the next line, and so does a bare carriage
// return before an option. An unbalanced quote runs to the end of the line,
// so the rest of that line belongs to the command. A quote escaped by a
// backslash starts a string that runs to the next escaped quote, as in sh -c
// "mysql -e \"a;b\" -pX", unless it follows a closing quote of the same
// kind (the quote idiom). A doubled backslash before a separator (\\|, \\;)
// is read as an escaped separator, as the text may be inside another string
// (JSON text, sh -c "..."), where it is one; in a plain shell it is a
// backslash and a separator, which is rare. It reads each byte once.
func commandEnd(s string, i int) int {
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\r':
			if n := crContinues(s, i); n > 0 {
				i = n
				continue
			}
			return i
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
				if escapedSeparator(s, i) {
					i += 3
					continue
				}
				i++
			}
		}
		i++
	}
	return len(s)
}

// crContinues reports whether the bare carriage return at s[i] is inside a
// command: it is not followed by a line feed, and it is between an option
// and its value or before the next option ("curl \r -u a:S3CR3T", "curl -u
// \r a:S3CR3T"). Any other carriage return starts a new line of output. It
// returns where the command continues (after the white space that follows),
// or 0 if the carriage return ends it.
func crContinues(s string, i int) int {
	if i+1 < len(s) && s[i+1] == '\n' {
		return 0
	}
	next := skipBlanks(s, i)
	if next < len(s) && s[next] == '-' {
		return next
	}
	// the word before it is an option that has no value attached
	j := i
	for j > 0 && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\r') {
		j--
	}
	k := j
	for k > 0 && s[k-1] != ' ' && s[k-1] != '\t' && !isLineEnd(s[k-1]) {
		k--
	}
	w := s[k:j]
	if len(w) == 2 && w[0] == '-' || strings.HasPrefix(w, "--") && !strings.Contains(w, "=") {
		return next
	}
	return 0
}

// redactOptions redacts the values of sensitive long options (--password
// S3CR3T) and the password options of the programs in textPrograms.
func redactOptions(s string) string {
	if strings.Contains(s, "--") {
		s = redactLongOptions(s)
	}
	if strings.Contains(s, "sshpass") {
		s = redactSshpass(s)
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

// sshpassStart matches sshpass and the white space after it.
var sshpassStart = regexp.MustCompile(`\bsshpass(?:[ \t\r]|\\\r?\n)`)

// skipBlanks returns the index of the first character at or after i that is
// not white space, a line continuation or a bare carriage return.
func skipBlanks(s string, i int) int {
	for i < len(s) {
		switch {
		case s[i] == ' ' || s[i] == '\t' || s[i] == '\r' && !(i+1 < len(s) && s[i+1] == '\n'):
			i++
		case lineContinuation(s, i) > 0:
			i += lineContinuation(s, i)
		default:
			return i
		}
	}
	return i
}

// redactSshpass redacts the password option of each sshpass in s. It reads
// the options of sshpass itself (see sshpassOption) up to the command it
// runs, an unquoted separator or the end of the line, so that the options of
// that command (ssh -p 22) are left alone and an option cluster
// (-vp S3CR3T), a quoted value and a value after other options are found.
func redactSshpass(s string) string {
	var b strings.Builder
	pos, written := 0, 0
	redact := func(vs, end int) {
		if w := s[vs:end]; w != "" {
			if r := secretArg(w, 0, metaNone); r != w {
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
		return programWordEnd(s, i, 0), true
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

// redactProgramOption redacts the word at the end of each match of p.re
// (less p.back bytes), the value of a program's password option, and of
// each match of p.opt after it on the same command.
func redactProgramOption(s string, p textProgram) string {
	var b strings.Builder
	pos, written := 0, 0
	redactAt := func(vs int) int {
		end := programWordEnd(s, vs, p.sep)
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
