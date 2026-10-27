package tools

import (
	"fmt"
	"sort"
	"strings"
)

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

	quoted := make([]string, 0, len(unknown))
	for i, key := range unknown {
		if i == maxReportedUnknownArgs {
			quoted = append(quoted, fmt.Sprintf("and %d more", len(unknown)-i))
			break
		}
		if r := []rune(key); len(r) > maxArgNameLen {
			key = string(r[:maxArgNameLen]) + "..."
		}
		quoted = append(quoted, fmt.Sprintf("%q", key))
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
	return fmt.Errorf("invalid arguments for tool %s: unknown %s %s (%s); nothing was executed", name, noun, strings.Join(quoted, ", "), valid)
}
