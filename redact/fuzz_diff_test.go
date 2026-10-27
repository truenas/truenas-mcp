package redact

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A differential fuzz harness for the command-line redaction in Text. It
// builds lines in the shapes that are hard to scan (secrets inside quoting
// wrappers, escaped quotes, separators, carriage returns, programs that run
// other programs) around a known secret marker and counts
//
//   - leaks: lines whose output still contains the marker, and
//   - over-redaction: lines with a "decoy" option of another program (ssh
//     -p2222) that the output lost.
//
// With no environment it runs a small fixed sample that must have no leak in
// the shapes that are handled, so it is cheap in CI. REDACT_FUZZ_LINES=N
// runs N lines per shape and prints the counts (use -v), for example
//
//	REDACT_FUZZ_LINES=20000 go test ./redact -run TestFuzzDifferential -v
//
// Run the same harness against another version of the package (copy this
// file next to it) to compare leak counts before and after a change.

const fuzzMarker = "ZQ9MARKER7"

// fuzzCase is one generated line.
type fuzzCase struct {
	shape string // the gap the line exercises
	line  string
	decoy string // text of another program's option that must stay visible, or ""
}

// quoting wrappers: how the inner shell command is embedded in the text.
var fuzzWrappers = []struct {
	name string
	wrap func(inner string) string
}{
	{"plain", func(s string) string { return s }},
	{"sq", func(s string) string { return "sh -c '" + strings.ReplaceAll(s, "'", `'\''`) + "'" }},
	{"dq", func(s string) string { return `sh -c "` + dqEscape(s) + `"` }},
	// outer-escaped: the shell line as it is written inside a JSON string.
	{"outer", func(s string) string { return jsonEscape(s) }},
	{"outer-dq", func(s string) string { return jsonEscape(`sh -c "` + dqEscape(s) + `"`) }},
}

func dqEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(s)
}

func jsonEscape(s string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) }

// fuzzSecrets are the secret values, as written in the inner shell command.
var fuzzSecrets = []string{fuzzMarker, "'" + fuzzMarker + " x'", `"` + fuzzMarker + ` x"`}

// fuzzPrograms build the inner shell command around a secret. The simple ones
// are the programs whose password option is read without any scope rules.
var fuzzPrograms = []struct {
	name   string
	sshpas bool
	cmd    func(secret string) string
}{
	{"mysql", false, func(s string) string { return "mysql -u root -p" + s + " db" }},
	{"curl", false, func(s string) string { return "curl -s -u adm:" + s + " https://h/x" }},
	{"smbclient", false, func(s string) string { return "smbclient //h/s -U adm%" + s }},
	{"ipmitool", false, func(s string) string { return "ipmitool -H h -P " + s + " power status" }},
	{"sshpass", true, func(s string) string { return "sshpass -p " + s + " ssh -p2222 u@h uptime" }},
	{"sshpass-att", true, func(s string) string { return "sshpass -p" + s + " ssh -p2222 u@h uptime" }},
	{"sshpass-e", true, func(s string) string { return "sshpass -e -f /tmp/f -p " + s + " ssh u@h" }},
	{"sshpass-cluster", true, func(s string) string { return "sshpass -vp " + s + " ssh u@h" }},
	{"sshpass-cluster2", true, func(s string) string { return "sshpass -ep" + s + " ssh -p 22 u@h" }},
	{"sshpass-sudo", true, func(s string) string { return "sudo -n sshpass -d 3 -P assword -p " + s + " scp -P 2222 f u@h:" }},
	{"sshpass-late", true, func(s string) string { return "sshpass -v -p " + s + " -- ssh -p 22 u@h" }},
	{"sshpass-first", true, func(s string) string { return "sshpass -p " + s + " -p2222" }},
	// a quoted password with a space, written as an attached or separate escaped
	// quote (the JSON form of -p"S 3CR3T"), or with a plain double quote
	{"sshpass-escq", true, func(string) string { return `sshpass -p\"` + fuzzMarker + ` x\" ssh -p2222 u@h` }},
	{"sshpass-escq-sep", true, func(string) string { return `sshpass -v -p \"` + fuzzMarker + ` x\" ssh -p2222 u@h` }},
	{"sshpass-dq-att", true, func(string) string { return `sshpass -vp"` + fuzzMarker + ` x" ssh -p2222 u@h` }},
	// a second sshpass after an option value with the quote idiom
	{"sshpass-idiom-2nd", true, func(s string) string {
		return `sshpass -f 'it'\''s' ssh u@h; sshpass -p ` + s + ` ssh -p2222 u@h`
	}},
	// an escaped separator in an option value
	{"sshpass-escsep", true, func(s string) string { return `sshpass -f a\\|b -p ` + s + ` ssh -p2222 u@h` }},
}

// noise before and after the command.
var (
	fuzzPre  = []string{"", "echo ok; ", "cd /x && ", "x | ", "env A=1 ", "nohup "}
	fuzzMid  = []string{"", ` -e 'a|b'`, ` -e "a;b"`, ` -e "a'|b"`, ` -e 'a"b'`, ` -e "a'b"`}
	fuzzPost = []string{"", ` \| x`, ` \; x`, ` '\'' x`, ` | grep x`, "; ls", ` && echo done`, " 2>&1", " &", "\r", "\r\n"}
	// fuzzGap are separators where a carriage return can be.
	fuzzGap = []string{" ", "\r ", " \r ", "\r"}
)

func genFuzzCase(r *rand.Rand, shape string) fuzzCase {
	pick := func(a []string) string { return a[r.Intn(len(a))] }
	prog := fuzzPrograms[r.Intn(len(fuzzPrograms))]
	w := fuzzWrappers[r.Intn(len(fuzzWrappers))]
	sec := fuzzSecrets[r.Intn(len(fuzzSecrets))]
	pre, mid, post := pick(fuzzPre), "", pick(fuzzPost)
	switch shape {
	case "sshpass":
		for !prog.sshpas {
			prog = fuzzPrograms[r.Intn(len(fuzzPrograms))]
		}
		w = fuzzWrappers[r.Intn(len(fuzzWrappers))] // also the JSON forms
		sec = fuzzSecrets[r.Intn(2)]
		post = pick(fuzzPost[:len(fuzzPost)-2])
	case "outer-escape":
		for prog.sshpas {
			prog = fuzzPrograms[r.Intn(len(fuzzPrograms))]
		}
		w = fuzzWrappers[3+r.Intn(2)]
		sec = fuzzSecrets[0]
		mid = pick(fuzzMid[1:]) + pick([]string{` \|`, ` \;`, ` '\''`})
	case "cr":
		for prog.sshpas {
			prog = fuzzPrograms[r.Intn(len(fuzzPrograms))]
		}
		w = fuzzWrappers[r.Intn(3)]
		sec = fuzzSecrets[0]
	case "idiom-user":
		for prog.name != "curl" && prog.name != "smbclient" {
			prog = fuzzPrograms[r.Intn(len(fuzzPrograms))]
		}
		w = fuzzWrappers[1] // the quote idiom is how a quote is written in sh -c '...'
		sec = fuzzSecrets[1]
	case "stray":
		for prog.name != "mysql" {
			prog = fuzzPrograms[r.Intn(len(fuzzPrograms))]
		}
		w = fuzzWrappers[0]
		sec = fuzzSecrets[0]
	case "mixed":
		mid = pick(fuzzMid)
	}
	cmd := prog.cmd(sec)
	if mid != "" && !prog.sshpas {
		cmd = strings.Replace(cmd, " -", mid+" -", 1)
	}
	if shape == "cr" {
		cmd = strings.ReplaceAll(cmd, " -", pick(fuzzGap)+"-")
		cmd = strings.Replace(cmd, " "+sec, pick(fuzzGap)+sec, 1)
	}
	decoy := ""
	if shape == "stray" {
		// An odd number of stray escaped quotes before a separator, then
		// another program with the same kind of option.
		stray := strings.Repeat(`\" `, 1+2*r.Intn(2))
		return fuzzCase{shape, cmd + " " + stray + "; ssh -p2222 h", "-p2222"}
	}
	inner := pre + cmd + post
	if strings.Contains(cmd, "ssh -p2222") {
		decoy = "-p2222"
	}
	return fuzzCase{shape, w.wrap(inner), decoy}
}

var fuzzShapes = []string{"sshpass", "outer-escape", "cr", "stray", "idiom-user", "mixed"}

// fuzzCounts runs n lines per shape and returns leaks and over-redaction by
// shape, plus examples of the first leak of each shape.
func fuzzCounts(n int, seed int64) (leaks, over map[string]int, example map[string]string) {
	leaks, over, example = map[string]int{}, map[string]int{}, map[string]string{}
	for _, shape := range fuzzShapes {
		r := rand.New(rand.NewSource(seed))
		for i := 0; i < n; i++ {
			c := genFuzzCase(r, shape)
			out := Text(c.line)
			if strings.Contains(out, fuzzMarker) {
				leaks[shape]++
				if example[shape] == "" {
					example[shape] = fmt.Sprintf("%q -> %q", c.line, out)
				}
			}
			if c.decoy != "" && !strings.Contains(out, c.decoy) {
				over[shape]++
			}
		}
	}
	return
}

// fuzzLeakFreeShapes are the shapes that must not leak at all. Add a shape
// here when a gap is closed. The "stray" shape only measures over-redaction,
// which is a known limit (see README), and it must not leak either.
var fuzzLeakFreeShapes = map[string]bool{
	"sshpass": true, "outer-escape": true, "cr": true, "stray": true, "idiom-user": true, "mixed": true,
}

func TestFuzzDifferential(t *testing.T) {
	n, verbose := 400, false
	if v := os.Getenv("REDACT_FUZZ_LINES"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n <= 0 {
			t.Fatalf("bad REDACT_FUZZ_LINES %q", v)
		}
		verbose = true
	}
	leaks, over, example := fuzzCounts(n, 1)
	shapes := append([]string(nil), fuzzShapes...)
	sort.Strings(shapes)
	for _, s := range shapes {
		if verbose {
			t.Logf("%-13s lines=%d leaks=%d over-redacted=%d", s, n, leaks[s], over[s])
			if example[s] != "" {
				t.Logf("  first leak: %s", example[s])
			}
		}
		if fuzzLeakFreeShapes[s] && leaks[s] > 0 {
			t.Errorf("shape %s: %d of %d lines leak the marker, e.g. %s", s, leaks[s], n, example[s])
		}
	}
}
