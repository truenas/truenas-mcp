package tools

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Confirmation tokens gate the destructive tools. A destructive call without
// a valid token runs the tool's preview and returns it with a token; the
// call executes only when it is repeated with the same arguments and that
// token. A token is random, single use, short-lived and held in memory
// only. It is bound to the tool, to a hash of the canonical JSON of the
// arguments (see confirmationArgsHash), and, once a tool is migrated to
// a writePlan, to a hash of that plan's method and params
// (see confirmationPlanHash).
//
// A token proves that a preview of exactly these arguments was produced
// shortly before. It does not prove that a human saw or approved it.

const (
	// confirmationTokenTTL is how long a token stays valid.
	confirmationTokenTTL = 5 * time.Minute
	// maxConfirmationTokens bounds the tokens held at once. When it is
	// reached, the token closest to expiry is dropped.
	maxConfirmationTokens = 1024
	// confirmationTokenBytes is the token's entropy (128 bits).
	confirmationTokenBytes = 16
)

// confirmationControlArgs are arguments that select how a destructive call
// runs, not what it does. They are left out of the argument hash and are
// never passed to a handler (dry_run is, for previews).
var confirmationControlArgs = map[string]bool{"confirm": true, "confirmation_token": true, "dry_run": true}

// errTokenRejected wraps every rejection reason.
var errTokenRejected = errors.New("confirmation token rejected")

type tokenEntry struct {
	tool     string
	argsHash [sha256.Size]byte
	planHash [sha256.Size]byte
	expires  time.Time
	used     bool
}

// confirmationTokens is the in-memory token store. Tokens are kept by their
// SHA-256, so the store never holds a usable token and a lookup does not
// compare token bytes.
type confirmationTokens struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[[sha256.Size]byte]*tokenEntry
}

func newConfirmationTokens() *confirmationTokens {
	return &confirmationTokens{now: time.Now, entries: map[[sha256.Size]byte]*tokenEntry{}}
}

// confirmationArgsHash hashes the canonical JSON of args without the
// control arguments. encoding/json writes object keys in sorted order at
// every level, and every number arrives as a float64, so the same
// arguments in any key order (or 5 and 5.0) hash the same.
func confirmationArgsHash(args map[string]interface{}) ([sha256.Size]byte, error) {
	canonical := make(map[string]interface{}, len(args))
	for k, v := range args {
		if !confirmationControlArgs[k] {
			canonical[k] = v
		}
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(data), nil
}

// confirmationPlanHash hashes a write plan's method and call params. The
// store keeps only this hash, never the params, which may contain secrets.
// encoding/json writes object keys in sorted order, so the same method and
// params hash the same.
func confirmationPlanHash(method string, params []interface{}) ([sha256.Size]byte, error) {
	data, err := json.Marshal(struct {
		Method string        `json:"method"`
		Params []interface{} `json:"params"`
	}{Method: method, Params: params})
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(data), nil
}

func (p *writePlan) planHash() ([sha256.Size]byte, error) {
	if p == nil {
		return [sha256.Size]byte{}, nil
	}
	if err := p.paramNamesOK(); err != nil {
		return [sha256.Size]byte{}, err
	}
	return confirmationPlanHash(p.Method, p.CallParams)
}

// prune drops expired entries and, at the cap, the entry closest to expiry.
// The caller holds mu.
func (c *confirmationTokens) prune(now time.Time) {
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	for len(c.entries) >= maxConfirmationTokens {
		var oldest [sha256.Size]byte
		var oldestExp time.Time
		first := true
		for k, e := range c.entries {
			if first || e.expires.Before(oldestExp) {
				oldest, oldestExp, first = k, e.expires, false
			}
		}
		delete(c.entries, oldest)
	}
}

// issue returns a new token for tool and argsHash, unbound to a plan.
// Destructive tools not yet migrated to a writePlan use this.
func (c *confirmationTokens) issue(tool string, argsHash [sha256.Size]byte) (string, error) {
	return c.issueWithPlan(tool, argsHash, [sha256.Size]byte{})
}

// issueWithPlan returns a new token bound to tool, argsHash, and planHash.
// planHash should be confirmationPlanHash of the previewed plan. A zero
// planHash means the token is not bound to a plan (legacy issue).
func (c *confirmationTokens) issueWithPlan(tool string, argsHash, planHash [sha256.Size]byte) (string, error) {
	b := make([]byte, confirmationTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("could not generate a confirmation token: %w", err)
	}
	tok := hex.EncodeToString(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.prune(now)
	c.entries[sha256.Sum256([]byte(tok))] = &tokenEntry{tool: tool, argsHash: argsHash, planHash: planHash, expires: now.Add(confirmationTokenTTL)}
	return tok, nil
}

// redeem consumes tok for tool and argsHash, with no plan binding.
func (c *confirmationTokens) redeem(tok, tool string, argsHash [sha256.Size]byte) error {
	return c.redeemWithPlan(tok, tool, argsHash, [sha256.Size]byte{})
}

// redeemWithPlan consumes tok for tool, argsHash, and planHash. A token
// presented for another tool, other arguments, or a different planned
// operation is consumed too, so it cannot be retried.
func (c *confirmationTokens) redeemWithPlan(tok, tool string, argsHash, planHash [sha256.Size]byte) error {
	key := sha256.Sum256([]byte(tok))
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	now := c.now()
	switch {
	case !ok:
		return fmt.Errorf("%w: it is unknown (it was not issued by this server, or it expired and was removed, or the server restarted)", errTokenRejected)
	case e.used:
		return fmt.Errorf("%w: it was already used; every token executes at most once", errTokenRejected)
	case !now.Before(e.expires):
		delete(c.entries, key)
		return fmt.Errorf("%w: it expired; tokens are valid for %s", errTokenRejected, confirmationTokenTTL)
	}
	e.used = true
	switch {
	case e.tool != tool:
		return fmt.Errorf("%w: it was issued for a different tool", errTokenRejected)
	case e.argsHash != argsHash:
		return fmt.Errorf("%w: it was issued for different arguments; repeat the previewed call with exactly the same arguments", errTokenRejected)
	case e.planHash != planHash:
		return fmt.Errorf("%w: it was issued for a different planned operation (the system state changed since the preview)", errTokenRejected)
	}
	return nil
}

// live reports whether tok is an issued token that is unused and unexpired.
func (c *confirmationTokens) live(tok string) bool {
	if len(tok) != 2*confirmationTokenBytes {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[sha256.Sum256([]byte(tok))]
	return ok && !e.used && c.now().Before(e.expires)
}

// IsConfirmationToken reports whether tok is a live token this registry
// issued. The server uses it to let only such a value through redaction
// (mcp.ConfirmationTokens).
func (r *Registry) IsConfirmationToken(tok string) bool {
	return r.confirmTokens.live(tok)
}
