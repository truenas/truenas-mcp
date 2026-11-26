package tools

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

// A password a tool generates (create_user or update_user with
// password_mode random) is returned once, in the result of the call that
// set it. The server redacts every password field in tool output, so the
// registry records the generated value here and the server lets it
// through only when TakeOneTimeSecret accepts it, which also forgets it.
// The store keeps the SHA-256 of each value, never the value itself.

// oneTimeSecretTTL bounds how long a generated password waits for the
// result that returns it.
const oneTimeSecretTTL = time.Minute

type oneTimeSecrets struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[[sha256.Size]byte]time.Time
}

func newOneTimeSecrets() *oneTimeSecrets {
	return &oneTimeSecrets{now: time.Now, entries: map[[sha256.Size]byte]time.Time{}}
}

func (s *oneTimeSecrets) add(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, exp := range s.entries {
		if !now.Before(exp) {
			delete(s.entries, k)
		}
	}
	s.entries[sha256.Sum256([]byte(v))] = now.Add(oneTimeSecretTTL)
}

func (s *oneTimeSecrets) take(v string) bool {
	if v == "" {
		return false
	}
	key := sha256.Sum256([]byte(v))
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.entries[key]
	delete(s.entries, key)
	return ok && s.now().Before(exp)
}

// TakeOneTimeSecret reports whether v is a password this registry
// generated and has not returned yet, and forgets it (mcp.OneTimeSecrets).
func (r *Registry) TakeOneTimeSecret(v string) bool {
	return r.oneTimeSecrets.take(v)
}

// generatedPasswordLen is the length of a generated password: about 150
// bits from the alphabet below.
const generatedPasswordLen = 24

// passwordClasses are the character classes of a generated password, one
// of each at least, so a password complexity rule set is met. Look-alike
// characters are left out; the symbols need no quoting in common shells.
var passwordClasses = []string{
	"ABCDEFGHJKLMNPQRSTUVWXYZ",
	"abcdefghijkmnopqrstuvwxyz",
	"23456789",
	"-_.@%+=",
}

func randIndex(n int) (int, error) {
	i, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(i.Int64()), nil
}

// generatePassword returns a random password from crypto/rand.
func generatePassword() (string, error) {
	all := strings.Join(passwordClasses, "")
	out := make([]byte, 0, generatedPasswordLen)
	for _, class := range passwordClasses {
		i, err := randIndex(len(class))
		if err != nil {
			return "", fmt.Errorf("could not generate a password: %w", err)
		}
		out = append(out, class[i])
	}
	for len(out) < generatedPasswordLen {
		i, err := randIndex(len(all))
		if err != nil {
			return "", fmt.Errorf("could not generate a password: %w", err)
		}
		out = append(out, all[i])
	}
	for i := len(out) - 1; i > 0; i-- {
		j, err := randIndex(i + 1)
		if err != nil {
			return "", fmt.Errorf("could not generate a password: %w", err)
		}
		out[i], out[j] = out[j], out[i]
	}
	return string(out), nil
}
