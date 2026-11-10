package tools

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
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

// Schema validation of argument values.
//
// validateArgs above checks names only. validateArgValues checks every
// declared argument against the tool's own input schema before anything
// runs: a wrong JSON type, a value outside an enum, a missing required
// argument or an out-of-range number is an error, not something a handler
// silently replaces with its default.
//
// Only the subset of JSON Schema the tool schemas use is implemented; see
// supportedSchemaKeywords. TestToolSchemasUseOnlySupportedKeywords fails if a
// schema starts using anything else, so a new keyword is never silently
// ignored.
//
// Errors name the argument path and what was expected, never the value:
// values may carry secrets.

// supportedSchemaKeywords are the JSON Schema keywords validateSchemaValue
// enforces, plus the annotations it deliberately ignores.
var supportedSchemaKeywords = map[string]bool{
	// Enforced.
	"type": true, "enum": true, "const": true, "required": true,
	"properties": true, "additionalProperties": true, "items": true,
	"minimum": true, "maximum": true, "minLength": true, "maxLength": true,
	"pattern": true,
	// Annotations only.
	"description": true, "default": true, "title": true, "examples": true,
}

const maxReportedEnumValues = 12

// schemaPatterns caches compiled "pattern" regular expressions.
var schemaPatterns sync.Map // string -> *regexp.Regexp

func validateArgValues(name string, tool Tool, args map[string]interface{}) error {
	msg := schemaViolation(nil, tool.Definition.InputSchema, args, true)
	if msg == "" {
		return nil
	}
	return fmt.Errorf("invalid arguments for tool %s: %s; nothing was executed", name, msg)
}

// nullRefusedArgs are top-level arguments whose null is never treated as
// omitted: a null dry_run or confirm is refused as not a boolean.
var nullRefusedArgs = map[string]bool{"dry_run": true, "confirm": true}

// dropNullOptionals treats null as omitted for optional arguments: it
// returns args without every declared, non-required key whose value is
// null, at the top level and inside nested objects (including objects in
// arrays). Required keys keep their null, so validation reports them as
// required; undeclared keys are kept, so they are still reported as
// unknown or not allowed; dry_run and confirm are kept, so a null is
// refused. args itself is never modified: a copy is made where anything
// is dropped.
func dropNullOptionals(schema map[string]interface{}, args map[string]interface{}) map[string]interface{} {
	out, _ := dropNullsInObject(schema, args, true)
	return out
}

func dropNullsInObject(schema map[string]interface{}, obj map[string]interface{}, top bool) (map[string]interface{}, bool) {
	props, _ := schema["properties"].(map[string]interface{})
	if len(obj) == 0 || len(props) == 0 {
		return obj, false
	}
	required := map[string]bool{}
	for _, k := range schemaRequired(schema["required"]) {
		required[k] = true
	}
	var out map[string]interface{}
	set := func(k string, v interface{}, drop bool) {
		if out == nil {
			out = make(map[string]interface{}, len(obj))
			for k2, v2 := range obj {
				out[k2] = v2
			}
		}
		if drop {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	for k, v := range obj {
		sub, declared := props[k].(map[string]interface{})
		if !declared {
			continue
		}
		if v == nil {
			if !required[k] && !(top && nullRefusedArgs[k]) {
				set(k, nil, true)
			}
			continue
		}
		if nv, changed := dropNullsInValue(sub, v); changed {
			set(k, nv, false)
		}
	}
	if out == nil {
		return obj, false
	}
	return out, true
}

func dropNullsInValue(schema map[string]interface{}, v interface{}) (interface{}, bool) {
	switch val := v.(type) {
	case map[string]interface{}:
		return dropNullsInObject(schema, val, false)
	case []interface{}:
		items, ok := schema["items"].(map[string]interface{})
		if !ok {
			return v, false
		}
		var out []interface{}
		for i, item := range val {
			if ni, changed := dropNullsInValue(items, item); changed {
				if out == nil {
					out = append([]interface{}(nil), val...)
				}
				out[i] = ni
			}
		}
		if out == nil {
			return v, false
		}
		return out, true
	}
	return v, false
}

// argPath is an argument path built lazily: its text is only produced
// when an error is reported.
type argPath struct {
	parent *argPath
	key    string // object key; used when index < 0
	index  int
}

func (p *argPath) child(key string) *argPath { return &argPath{parent: p, key: key, index: -1} }
func (p *argPath) item(i int) *argPath       { return &argPath{parent: p, index: i} }

// String renders the path. A key that is not made of plain characters is
// not echoed: it comes from the client and may be anything.
func (p *argPath) String() string {
	if p == nil {
		return "arguments"
	}
	if p.index >= 0 {
		return fmt.Sprintf("%s[%d]", p.parent.String(), p.index)
	}
	key := p.key
	if !safeArgName.MatchString(key) || len(key) > maxArgNameLen {
		key = "(field with invalid characters)"
	}
	if p.parent == nil {
		return key
	}
	return p.parent.String() + "." + key
}

// schemaViolation returns a short description of the first way v does not
// match schema, or "" when it matches. path is the argument path so far
// (nil at the top level). Properties are visited in sorted order so the
// message is deterministic.
func schemaViolation(path *argPath, schema map[string]interface{}, v interface{}, top bool) string {
	if schema == nil {
		return ""
	}
	if t, ok := schema["type"]; ok {
		if !top || v != nil {
			if !matchesSchemaType(t, v) {
				if r := boundedIntegerText(t, schema); r != "" {
					return fmt.Sprintf("%s must be %s", path, r)
				}
				return fmt.Sprintf("%s must be %s", path, describeSchemaType(t))
			}
		}
	}
	if top && v == nil {
		// nil top-level arguments are an empty object.
		v = map[string]interface{}{}
	}
	if enum, ok := schema["enum"].([]interface{}); ok && !inEnum(enum, v) {
		return fmt.Sprintf("%s must be one of %s", path, enumList(enum))
	}
	if enum, ok := schema["enum"].([]string); ok {
		list := make([]interface{}, len(enum))
		for i, e := range enum {
			list[i] = e
		}
		if !inEnum(list, v) {
			return fmt.Sprintf("%s must be one of %s", path, enumList(list))
		}
	}
	if c, ok := schema["const"]; ok && !schemaEqual(c, v) {
		return fmt.Sprintf("%s must be %s", path, enumList([]interface{}{c}))
	}

	switch val := v.(type) {
	case string:
		n := utf8.RuneCountInString(val)
		if min, ok := schemaNumber(schema["minLength"]); ok && float64(n) < min {
			return fmt.Sprintf("%s must be at least %s characters long", path, formatSchemaNumber(min))
		}
		if max, ok := schemaNumber(schema["maxLength"]); ok && float64(n) > max {
			return fmt.Sprintf("%s must be at most %s characters long", path, formatSchemaNumber(max))
		}
		if p, ok := schema["pattern"].(string); ok {
			re, err := schemaPattern(p)
			if err != nil || !re.MatchString(val) {
				return fmt.Sprintf("%s does not match the required pattern %s", path, p)
			}
		}
	case map[string]interface{}:
		return objectViolation(path, schema, val)
	case []interface{}:
		if items, ok := schema["items"].(map[string]interface{}); ok {
			for i, item := range val {
				if msg := schemaViolation(path.item(i), items, item, false); msg != "" {
					return msg
				}
			}
		}
	default:
		if f, ok := jsonNumber(v); ok {
			min, hasMin := schemaNumber(schema["minimum"])
			max, hasMax := schemaNumber(schema["maximum"])
			if (hasMin && f < min) || (hasMax && f > max) {
				if r := boundedIntegerText(schema["type"], schema); r != "" {
					return fmt.Sprintf("%s must be %s", path, r)
				}
				if hasMin && f < min {
					return fmt.Sprintf("%s must be at least %s", path, formatSchemaNumber(min))
				}
				return fmt.Sprintf("%s must be at most %s", path, formatSchemaNumber(max))
			}
		}
	}
	return ""
}

func objectViolation(path *argPath, schema map[string]interface{}, obj map[string]interface{}) string {
	props, _ := schema["properties"].(map[string]interface{})
	for _, key := range schemaRequired(schema["required"]) {
		// A required argument given as null is reported as missing.
		if v, ok := obj[key]; !ok || v == nil {
			return fmt.Sprintf("%s is required", path.child(key))
		}
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		sub, declared := props[key].(map[string]interface{})
		if !declared {
			switch ap := schema["additionalProperties"].(type) {
			case bool:
				if !ap && path != nil {
					// Top-level unknown names are reported by validateArgs.
					return fmt.Sprintf("%s is not an allowed field", path.child(key))
				}
				continue
			case map[string]interface{}:
				sub = ap
			default:
				continue
			}
		}
		if msg := schemaViolation(path.child(key), sub, obj[key], false); msg != "" {
			return msg
		}
	}
	return ""
}

func schemaRequired(v interface{}) []string {
	switch r := v.(type) {
	case []string:
		return r
	case []interface{}:
		out := make([]string, 0, len(r))
		for _, s := range r {
			if str, ok := s.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// matchesSchemaType reports whether v has JSON type t (a type name or a list
// of them). Arguments decoded from JSON hold float64 numbers, so an integer
// is a whole, finite float64; NaN, infinities and fractions are not.
func matchesSchemaType(t interface{}, v interface{}) bool {
	switch tt := t.(type) {
	case string:
		return matchesTypeName(tt, v)
	case []string:
		for _, name := range tt {
			if matchesTypeName(name, v) {
				return true
			}
		}
	case []interface{}:
		for _, name := range tt {
			if s, ok := name.(string); ok && matchesTypeName(s, v) {
				return true
			}
		}
	}
	return false
}

func matchesTypeName(name string, v interface{}) bool {
	switch name {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]interface{})
		return ok
	case "array":
		_, ok := v.([]interface{})
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := jsonNumber(v)
		return ok
	case "integer":
		f, ok := jsonNumber(v)
		return ok && f == math.Trunc(f)
	}
	return false
}

// boundedIntegerText describes an integer argument with both a minimum and
// a maximum as "an integer from MIN to MAX", so a wrong type and an
// out-of-range value get the same, complete answer. It returns "" for any
// other schema.
func boundedIntegerText(t interface{}, schema map[string]interface{}) string {
	isInt := false
	switch tt := t.(type) {
	case string:
		isInt = tt == "integer"
	case []string:
		for _, s := range tt {
			isInt = isInt || s == "integer"
		}
	case []interface{}:
		for _, s := range tt {
			isInt = isInt || s == "integer"
		}
	}
	min, hasMin := schemaNumber(schema["minimum"])
	max, hasMax := schemaNumber(schema["maximum"])
	if !isInt || !hasMin || !hasMax {
		return ""
	}
	return fmt.Sprintf("an integer from %s to %s", formatSchemaNumber(min), formatSchemaNumber(max))
}

func describeSchemaType(t interface{}) string {
	article := func(name string) string {
		switch name {
		case "integer":
			return "an integer (a whole number)"
		case "object", "array":
			return "an " + name
		case "boolean":
			return "a boolean (true or false)"
		case "null":
			return "null"
		}
		return "a " + name
	}
	switch tt := t.(type) {
	case string:
		return article(tt)
	case []string:
		parts := make([]string, len(tt))
		for i, s := range tt {
			parts[i] = article(s)
		}
		return strings.Join(parts, " or ")
	case []interface{}:
		parts := make([]string, 0, len(tt))
		for _, s := range tt {
			if str, ok := s.(string); ok {
				parts = append(parts, article(str))
			}
		}
		return strings.Join(parts, " or ")
	}
	return "of the declared type"
}

// jsonNumber returns v as a finite float64 if it is a number.
func jsonNumber(v interface{}) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case float32:
		f = float64(n)
	case int:
		f = float64(n)
	case int32:
		f = float64(n)
	case int64:
		f = float64(n)
	case json.Number:
		parsed, err := n.Float64()
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func formatSchemaNumber(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func schemaNumber(v interface{}) (float64, bool) {
	return jsonNumber(v)
}

func inEnum(enum []interface{}, v interface{}) bool {
	for _, e := range enum {
		if schemaEqual(e, v) {
			return true
		}
	}
	return false
}

// schemaEqual compares a schema constant with an argument value. Numbers
// compare by value, whatever their Go type.
func schemaEqual(a, b interface{}) bool {
	if fa, ok := jsonNumber(a); ok {
		fb, ok := jsonNumber(b)
		return ok && fa == fb
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	}
	return false
}

// enumList renders the allowed values (they come from the schema, not the
// client), bounded so a long enum does not inflate the message.
func enumList(enum []interface{}) string {
	parts := make([]string, 0, len(enum))
	for i, e := range enum {
		if i == maxReportedEnumValues {
			parts = append(parts, fmt.Sprintf("and %d more", len(enum)-i))
			break
		}
		if s, ok := e.(string); ok {
			parts = append(parts, strconv.Quote(s))
		} else if f, ok := jsonNumber(e); ok {
			parts = append(parts, formatSchemaNumber(f))
		} else {
			parts = append(parts, fmt.Sprint(e))
		}
	}
	return strings.Join(parts, ", ")
}

func schemaPattern(p string) (*regexp.Regexp, error) {
	if re, ok := schemaPatterns.Load(p); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, err
	}
	schemaPatterns.Store(p, re)
	return re, nil
}
