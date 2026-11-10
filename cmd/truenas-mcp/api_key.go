package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
)

// resolveAPIKey returns the API key from exactly one configured source.
// A non-empty --api-key or --api-key-file wins over the environment; the two
// flags must not be set together, and neither may TRUENAS_API_KEY and
// TRUENAS_API_KEY_FILE. lookupEnv has the same signature as os.Getenv.
func resolveAPIKey(flagKey, flagFile string, lookupEnv func(string) string) (string, error) {
	if flagKey != "" && flagFile != "" {
		return "", fmt.Errorf("provide only one of --api-key or --api-key-file")
	}
	if flagKey != "" {
		return flagKey, nil
	}
	if flagFile != "" {
		return readAPIKeyFile(flagFile)
	}

	envKey := lookupEnv("TRUENAS_API_KEY")
	envFile := lookupEnv("TRUENAS_API_KEY_FILE")
	if envKey != "" && envFile != "" {
		return "", fmt.Errorf("provide only one of TRUENAS_API_KEY or TRUENAS_API_KEY_FILE")
	}
	if envKey != "" {
		return envKey, nil
	}
	if envFile != "" {
		return readAPIKeyFile(envFile)
	}
	return "", nil
}

// readAPIKeyFile reads an API key from path, trimming one trailing newline
// (including a Windows CRLF). On Unix it refuses a file that is readable by
// group or others. Errors name the path but never include the file contents.
func readAPIKeyFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("cannot read API key file %s: %w", path, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("cannot stat API key file %s: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("API key file %s must be a regular file", path)
	}
	if keyFileTooOpen(st.Mode()) {
		return "", fmt.Errorf("API key file %s is readable by group or others (mode %04o); chmod 600 the file and retry", path, st.Mode().Perm())
	}

	b, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("cannot read API key file %s: %w", path, err)
	}
	key := trimOneTrailingNewline(b)
	if key == "" {
		return "", fmt.Errorf("API key file %s is empty", path)
	}
	return key, nil
}

// keyFileTooOpen reports whether a Unix file is readable by group or others.
// Windows has no equivalent permission bits, so the check is skipped there.
func keyFileTooOpen(mode os.FileMode) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	return mode.Perm()&0o077 != 0
}

// trimOneTrailingNewline removes a single trailing \n, or a trailing \r\n
// counted as one line ending. Any further trailing newlines are kept.
func trimOneTrailingNewline(b []byte) string {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
		if n = len(b); n > 0 && b[n-1] == '\r' {
			b = b[:n-1]
		}
	}
	return string(b)
}
