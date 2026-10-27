package tools

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// safeArgName matches the argument names an error may echo back.
var safeArgName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

const (
	// maxReportedUnknownArgs bounds how many unknown argument names an
	// error lists, and maxArgNameLen how much of each name it echoes, so a
	// hostile or confused client cannot inflate the message.
	maxReportedUnknownArgs = 10
	maxArgNameLen          = 64
)

// validateArgs rejects top-level arguments that the tool's input schema does
// not declare under "properties". Without it a misspelled or unsupported
// argument (say "limt" for "limit") is silently ignored and the call runs
// with the defaults, which for a write tool can mean doing something other
// than what was asked.
//
// Only names are checked and reported, never values: values may carry
// secrets. Tools whose schema sets additionalProperties to true (or to a
// schema object) stay permissive. Nested objects are not validated here.
func validateArgs(name string, tool Tool, args map[string]interface{}) error {
	if len(args) == 0 {
		return nil
	}
	schema := tool.Definition.InputSchema
	switch ap := schema["additionalProperties"].(type) {
	case bool:
		if ap {
			return nil
		}
	case map[string]interface{}:
		return nil
	}
	props, _ := schema["properties"].(map[string]interface{})

	var unknown []string
	for key := range args {
		if _, ok := props[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)

	// Only names made of plain characters are echoed. Anything else is
	// counted instead: a name containing, say, something the redaction
	// layer treats as sensitive could otherwise truncate the rest of the
	// message, and control or bidi characters have no business in it.
	var plain []string
	invalid := 0
	for _, key := range unknown {
		if !safeArgName.MatchString(key) {
			invalid++
			continue
		}
		plain = append(plain, key)
	}
	quoted := make([]string, 0, len(plain)+1)
	invalidMsg := ""
	for i, key := range plain {
		if i == maxReportedUnknownArgs {
			quoted = append(quoted, fmt.Sprintf("and %d more", len(plain)-i))
			break
		}
		if len(key) > maxArgNameLen {
			key = key[:maxArgNameLen] + "..."
		}
		quoted = append(quoted, fmt.Sprintf("%q", key))
	}
	if invalid > 0 {
		noun := "keys"
		if invalid == 1 {
			noun = "key"
		}
		invalidMsg = fmt.Sprintf("%d %s with invalid characters", invalid, noun)
	}

	noun := "argument"
	if len(unknown) > 1 {
		noun = "arguments"
	}
	valid := "this tool takes no arguments"
	if len(props) > 0 {
		names := make([]string, 0, len(props))
		for k := range props {
			names = append(names, k)
		}
		sort.Strings(names)
		valid = "valid arguments: " + strings.Join(names, ", ")
	}
	detail := " " + strings.Join(quoted, ", ")
	switch {
	case len(quoted) == 0:
		detail = ": " + invalidMsg
	case invalidMsg != "":
		detail += " and " + invalidMsg
	}
	return fmt.Errorf("invalid arguments for tool %s: unknown %s%s (%s); nothing was executed", name, noun, detail, valid)
}

// withMigrationHint adds to an unknown-argument error the tool-specific
// pointer to where a renamed argument went. configure_directory_service no
// longer takes the flat form (type, domain, bindname, ...); naming the old
// arguments and the new shape tells a client with a stale schema how to fix
// the call. Only argument names from a fixed list are echoed, never values.
func withMigrationHint(name string, args map[string]interface{}, err error) error {
	if name != "configure_directory_service" {
		return err
	}
	if flat := flatArgumentNames(args, dsUpdateTopLevelFields()); len(flat) > 0 {
		return fmt.Errorf("%w; the arguments of configure_directory_service changed: %s are no longer top-level arguments. Use service_type + credential + configuration (see the tool description)", err, strings.Join(flat, ", "))
	}
	return err
}
