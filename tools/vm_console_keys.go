package tools

import (
	"bytes"
	"fmt"
	"strings"
)

const (
	// vmConsoleMaxPayloadBytes caps the encoded byte length of a single
	// send_vm_console_keys call so a client cannot flood the guest serial PTY.
	vmConsoleMaxPayloadBytes = 4096
	// vmConsoleMaxKeys caps the number of named keys in a single call.
	vmConsoleMaxKeys = 256
)

const supportedVMConsoleKeysHint = "Enter, Tab, Shift+Tab, Backspace, Esc, Space, Up, Down, Left, Right, Home, End, Insert, Delete, PageUp, PageDown, F1-F12, Ctrl+<A-Z|@|[|\\|]|^|_|?>, Alt+<char|key>"

// encodeVMConsoleInput encodes literal text followed by named keys into raw
// terminal bytes. appCursorKeys selects DECCKM application-cursor sequences
// (ESC O ...) over normal CSI sequences (ESC [ ...) for arrows, Home, and End.
//
// Error messages intentionally omit the caller's text and raw key strings so
// secrets passed to send_vm_console_keys are never echoed back or logged.
func encodeVMConsoleInput(text string, keys []string, appCursorKeys bool) ([]byte, error) {
	if text == "" && len(keys) == 0 {
		return nil, fmt.Errorf("provide text, keys, or both")
	}
	if len(text) > vmConsoleMaxPayloadBytes {
		return nil, fmt.Errorf("console input exceeds maximum payload size of %d bytes", vmConsoleMaxPayloadBytes)
	}
	if len(keys) > vmConsoleMaxKeys {
		return nil, fmt.Errorf("too many keys (%d): maximum is %d per call", len(keys), vmConsoleMaxKeys)
	}

	var buf bytes.Buffer
	buf.Grow(len(text) + len(keys)*4)
	if text != "" {
		buf.WriteString(text)
	}

	for i, key := range keys {
		seq, ok := encodeNamedConsoleKey(key, appCursorKeys)
		if !ok {
			return nil, fmt.Errorf("keys[%d]: unsupported key name; supported keys: %s", i, supportedVMConsoleKeysHint)
		}
		if buf.Len()+len(seq) > vmConsoleMaxPayloadBytes {
			return nil, fmt.Errorf("console input exceeds maximum payload size of %d bytes", vmConsoleMaxPayloadBytes)
		}
		buf.Write(seq)
	}
	return buf.Bytes(), nil
}

func encodeNamedConsoleKey(raw string, appCursorKeys bool) ([]byte, bool) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return nil, false
	}
	lower := strings.ToLower(key)

	// Handle Ctrl+ / Control+ / C- combinations.
	for _, prefix := range []string{"ctrl+", "control+", "c-"} {
		if strings.HasPrefix(lower, prefix) {
			return encodeCtrlKey(key[len(prefix):])
		}
	}

	// Handle Alt+ / Meta+ combinations.
	for _, prefix := range []string{"alt+", "meta+"} {
		if strings.HasPrefix(lower, prefix) {
			return encodeAltKey(key[len(prefix):], appCursorKeys)
		}
	}

	return encodeBaseNamedKey(lower, appCursorKeys)
}

func encodeBaseNamedKey(lower string, appCursorKeys bool) ([]byte, bool) {
	switch lower {
	case "enter", "return", "cr":
		return []byte{'\r'}, true
	case "tab":
		return []byte{'\t'}, true
	case "shift+tab", "backtab", "btab":
		return []byte("\x1b[Z"), true
	case "backspace", "bs":
		return []byte{0x7f}, true
	case "esc", "escape":
		return []byte{0x1b}, true
	case "space":
		return []byte{' '}, true

	case "up":
		if appCursorKeys {
			return []byte("\x1bOA"), true
		}
		return []byte("\x1b[A"), true
	case "down":
		if appCursorKeys {
			return []byte("\x1bOB"), true
		}
		return []byte("\x1b[B"), true
	case "right":
		if appCursorKeys {
			return []byte("\x1bOC"), true
		}
		return []byte("\x1b[C"), true
	case "left":
		if appCursorKeys {
			return []byte("\x1bOD"), true
		}
		return []byte("\x1b[D"), true
	case "home":
		if appCursorKeys {
			return []byte("\x1bOH"), true
		}
		return []byte("\x1b[H"), true
	case "end":
		if appCursorKeys {
			return []byte("\x1bOF"), true
		}
		return []byte("\x1b[F"), true

	case "insert", "ins":
		return []byte("\x1b[2~"), true
	case "delete", "del":
		return []byte("\x1b[3~"), true
	case "pageup", "pgup", "page_up":
		return []byte("\x1b[5~"), true
	case "pagedown", "pgdown", "page_down":
		return []byte("\x1b[6~"), true

	case "f1":
		return []byte("\x1bOP"), true
	case "f2":
		return []byte("\x1bOQ"), true
	case "f3":
		return []byte("\x1bOR"), true
	case "f4":
		return []byte("\x1bOS"), true
	case "f5":
		return []byte("\x1b[15~"), true
	case "f6":
		return []byte("\x1b[17~"), true
	case "f7":
		return []byte("\x1b[18~"), true
	case "f8":
		return []byte("\x1b[19~"), true
	case "f9":
		return []byte("\x1b[20~"), true
	case "f10":
		return []byte("\x1b[21~"), true
	case "f11":
		return []byte("\x1b[23~"), true
	case "f12":
		return []byte("\x1b[24~"), true
	default:
		return nil, false
	}
}

func encodeCtrlKey(rest string) ([]byte, bool) {
	rest = strings.TrimSpace(rest)
	lower := strings.ToLower(rest)
	if lower == "space" {
		return []byte{0x00}, true
	}
	if len(rest) != 1 {
		return nil, false
	}
	ch := rest[0]
	switch {
	case ch >= 'a' && ch <= 'z':
		return []byte{ch - 'a' + 1}, true
	case ch >= 'A' && ch <= 'Z':
		return []byte{ch - 'A' + 1}, true
	case ch == '@':
		return []byte{0x00}, true
	case ch == '[':
		return []byte{0x1b}, true
	case ch == '\\':
		return []byte{0x1c}, true
	case ch == ']':
		return []byte{0x1d}, true
	case ch == '^':
		return []byte{0x1e}, true
	case ch == '_':
		return []byte{0x1f}, true
	case ch == '?':
		return []byte{0x7f}, true
	default:
		return nil, false
	}
}

func encodeAltKey(rest string, appCursorKeys bool) ([]byte, bool) {
	rest = strings.TrimSpace(rest)
	if len(rest) == 1 && rest[0] >= 0x20 && rest[0] <= 0x7e {
		return []byte{0x1b, rest[0]}, true
	}
	if seq, ok := encodeBaseNamedKey(strings.ToLower(rest), appCursorKeys); ok {
		out := make([]byte, 1+len(seq))
		out[0] = 0x1b
		copy(out[1:], seq)
		return out, true
	}
	return nil, false
}
