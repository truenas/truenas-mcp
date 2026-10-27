// Package safego holds helpers for background goroutines, where an
// unrecovered panic would take down the whole process.
package safego

import (
	"fmt"
	"runtime"
)

// Describe renders a recovered panic value for a log line. Only runtime
// errors (nil dereference, index out of range, ...) are quoted, because they
// carry no argument values; for anything else only the type is reported, as
// an arbitrary panic value could contain a secret.
func Describe(p any) string {
	if re, ok := p.(runtime.Error); ok {
		return re.Error()
	}
	return fmt.Sprintf("%T", p)
}
