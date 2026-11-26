package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// setupIO is everything setup reads from or writes to outside the NAS
// connection, so tests can drive it without a terminal or the real
// environment.
type setupIO struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	// isTTY reports whether stdin is an interactive terminal; prompts and
	// typed confirmations are only offered then.
	isTTY bool
	// readHidden prompts on stderr and reads one line from the terminal
	// without echo.
	readHidden func(prompt string) ([]byte, error)
	// readVisible prompts on stderr and reads one echoed line.
	readVisible func(prompt string) (string, error)

	lookupEnv     func(string) (string, bool)
	unsetenv      func(string) error
	hostname      func() (string, error)
	userConfigDir func() (string, error)
	executable    func() (string, error)
	now           func() time.Time
}

func realSetupIO() *setupIO {
	return &setupIO{
		stdin:         os.Stdin,
		stdout:        os.Stdout,
		stderr:        os.Stderr,
		isTTY:         term.IsTerminal(int(os.Stdin.Fd())),
		readHidden:    readHiddenTTY,
		readVisible:   func(prompt string) (string, error) { return readVisibleLine(os.Stdin, os.Stderr, prompt) },
		lookupEnv:     os.LookupEnv,
		unsetenv:      os.Unsetenv,
		hostname:      os.Hostname,
		userConfigDir: os.UserConfigDir,
		executable:    os.Executable,
		now:           time.Now,
	}
}

// readHiddenTTY reads a line from the terminal on stdin with echo off. An
// interrupt while the prompt is open restores the terminal before exiting,
// so the shell is not left without echo.
func readHiddenTTY(prompt string) ([]byte, error) {
	fd := int(os.Stdin.Fd())
	fprint(os.Stderr, prompt)
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			_ = term.Restore(fd, state)
			fprintln(os.Stderr)
			os.Exit(130)
		case <-done:
		}
	}()
	b, err := term.ReadPassword(fd)
	close(done)
	signal.Stop(sig)
	fprintln(os.Stderr)
	return b, err
}

func readVisibleLine(r io.Reader, w io.Writer, prompt string) (string, error) {
	fprint(w, prompt)
	b, err := readLine(r)
	s := string(b)
	wipe(b)
	return s, err
}

// maxSecretLine bounds a line read from stdin.
const maxSecretLine = 4096

var errEmptyInput = errors.New("no input (end of file)")

// readLine reads one line from r a byte at a time, so nothing after the
// newline is consumed (a second line can hold the OTP) and no buffer the
// caller cannot wipe holds the line. The trailing \n or \r\n is dropped.
func readLine(r io.Reader) ([]byte, error) {
	buf := make([]byte, 0, 128)
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				break
			}
			if len(buf) >= maxSecretLine {
				wipe(buf)
				return nil, fmt.Errorf("input line longer than %d bytes", maxSecretLine)
			}
			buf = appendWiping(buf, one[0])
		}
		if err == io.EOF {
			if len(buf) == 0 {
				return nil, errEmptyInput
			}
			break
		}
		if err != nil {
			wipe(buf)
			return nil, err
		}
	}
	one[0] = 0
	if n := len(buf); n > 0 && buf[n-1] == '\r' {
		buf[n-1] = 0
		buf = buf[:n-1]
	}
	return buf, nil
}

// appendWiping appends c, wiping the old backing array when append has to
// grow it, so no stale copy of a partial secret is left behind.
func appendWiping(buf []byte, c byte) []byte {
	if len(buf) < cap(buf) {
		return append(buf, c)
	}
	grown := make([]byte, len(buf), 2*cap(buf)+1)
	copy(grown, buf)
	wipe(buf)
	return append(grown, c)
}

// wipe overwrites b with zeros. This is best effort: Go may have copied
// the bytes elsewhere (for example into the string sent to TrueNAS, the
// encoded request, or an environment variable's string), and those copies
// cannot be cleared.
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// takeEnv reads an environment variable once and unsets it at once, so
// the value does not linger in this process's environment or reach a
// child process. The string the runtime holds cannot be wiped.
func takeEnv(sio *setupIO, name string) []byte {
	v, ok := sio.lookupEnv(name)
	if !ok {
		return nil
	}
	_ = sio.unsetenv(name)
	return []byte(v)
}

// Terminal output is best effort: a closed stderr must not change the
// outcome of setup.
func fprintf(w io.Writer, format string, args ...interface{}) { _, _ = fmt.Fprintf(w, format, args...) }
func fprintln(w io.Writer, args ...interface{})               { _, _ = fmt.Fprintln(w, args...) }
func fprint(w io.Writer, args ...interface{})                 { _, _ = fmt.Fprint(w, args...) }

// checkBoolFlagValues refuses a value given to a boolean flag with "=" that
// is not a boolean, such as --password-stdin=<password>, before the flag
// package would echo the value in its error. Parsing stops at "--" and at
// the first argument that is not a flag, as in the flag package. An
// argument the flag package rejects as bad syntax (three or more dashes,
// or "=" right after the dashes) is refused here too, because its error
// would echo the whole argument.
func checkBoolFlagValues(fs *flag.FlagSet, args []string) *setupError {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || len(a) < 2 || a[0] != '-' {
			return nil
		}
		name := strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
		if name == "" || name[0] == '-' || name[0] == '=' {
			return failf(exitSetupUsage, "bad flag syntax in argument %d; the argument was not shown", i+1)
		}
		eq := strings.IndexByte(name, '=')
		if eq < 0 {
			if fl := fs.Lookup(name); fl != nil && !isBoolFlag(fl) {
				i++ // skip the value
			}
			continue
		}
		fl := fs.Lookup(name[:eq])
		if fl == nil || !isBoolFlag(fl) {
			continue
		}
		if _, err := strconv.ParseBool(name[eq+1:]); err != nil {
			// The value is not quoted: it may be a password.
			return failf(exitSetupUsage, "--%s takes no value (it is a switch); the value given was not shown. There is no password flag: use the prompt, %s or --password-stdin", fl.Name, setupPasswordEnv)
		}
	}
	return nil
}

func isBoolFlag(fl *flag.Flag) bool {
	b, ok := fl.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}
