package tools

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIssue112ParseJobIDTable is the #112 regression: one helper returns
// (int, error) and errors on a missing or invalid id.
func TestIssue112ParseJobIDTable(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"42", 42, false},
		{"[42]", 42, false},
		{"[]", 0, true},
		{"null", 0, true},
		{`{"id":1}`, 0, true},
		{"0", 0, true},
		{"[0]", 0, true},
		{"", 0, true},
		{`"x"`, 0, true},
		{"4.5", 0, true},
		{"[7,8]", 7, false}, // first element, as the previous helper did
		{"42.0", 42, false}, // whole JSON number, as parseScrubJobID accepted
	}
	for _, tc := range tests {
		got, err := parseJobID(json.RawMessage(tc.in))
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseJobID(%q) = %d, nil, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseJobID(%q) = %d, %v, want %d, nil", tc.in, got, err, tc.want)
		}
	}
}

// TestIssue112JobIDParsingIsCentralized checks that tools/*.go (except
// tests) do not still copy-paste job-id unmarshalling; they must call
// parseJobID or a thin wrapper around it.
func TestIssue112JobIDParsingIsCentralized(t *testing.T) {
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var leftover []string
	for _, name := range entries {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Name == nil || fn.Name.Name == "parseJobID" {
				return true
			}
			if inlineJobIDUnmarshal(fn) {
				leftover = append(leftover, fset.Position(fn.Pos()).String()+": "+fn.Name.Name)
			}
			return true
		})
	}
	if len(leftover) > 0 {
		t.Errorf("job-id parsing still inlined (want a single parseJobID helper):\n  %s", strings.Join(leftover, "\n  "))
	}
}

// inlineJobIDUnmarshal reports whether fn still json.Unmarshals a method
// result into an int or []int, instead of calling parseJobID. The
// destination variable name does not matter.
func inlineJobIDUnmarshal(fn *ast.FuncDecl) bool {
	if fn.Body == nil {
		return false
	}
	intVars := map[string]bool{}
	callsParse := false
	unmarshals := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ValueSpec:
			if !isIntOrIntSliceType(x.Type) {
				return true
			}
			for _, ident := range x.Names {
				intVars[ident.Name] = true
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "json" && sel.Sel.Name == "Unmarshal" && len(x.Args) == 2 {
					if dest, ok := unmarshalDestIdent(x.Args[1]); ok && intVars[dest] {
						unmarshals = true
					}
				}
			}
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "parseJobID" {
				callsParse = true
			}
		}
		return true
	})
	if callsParse {
		return false
	}
	return unmarshals
}

func isIntOrIntSliceType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == "int"
	case *ast.ArrayType:
		elem, ok := t.Elt.(*ast.Ident)
		return ok && elem.Name == "int"
	default:
		return false
	}
}

func unmarshalDestIdent(arg ast.Expr) (string, bool) {
	unary, ok := arg.(*ast.UnaryExpr)
	if !ok || unary.Op != token.AND {
		return "", false
	}
	id, ok := unary.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}

func parseFuncDecl(t *testing.T, src, name string) *ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name != nil && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("function %s not found in snippet", name)
	return nil
}

// TestIssue112JobIDParsingDetectorCatchesGenericIntVar is the review
// follow-up: the centralized check must flag any json.Unmarshal of a
// method result into an int or []int, not only variables named jobID /
// jobIDArray / jobIDs.
func TestIssue112JobIDParsingDetectorCatchesGenericIntVar(t *testing.T) {
	intSrc := `package p
import "encoding/json"
func f(result json.RawMessage) {
	var n int
	_ = json.Unmarshal(result, &n)
}
`
	if !inlineJobIDUnmarshal(parseFuncDecl(t, intSrc, "f")) {
		t.Error("detector missed json.Unmarshal of a method result into an int not named jobID")
	}
	sliceSrc := `package p
import "encoding/json"
func f(result json.RawMessage) {
	var ids []int
	_ = json.Unmarshal(result, &ids)
}
`
	if !inlineJobIDUnmarshal(parseFuncDecl(t, sliceSrc, "f")) {
		t.Error("detector missed json.Unmarshal of a method result into []int")
	}
	countSrc := `package p
import "encoding/json"
func queryCount(raw json.RawMessage) {
	var n float64
	_ = json.Unmarshal(raw, &n)
}
`
	if inlineJobIDUnmarshal(parseFuncDecl(t, countSrc, "queryCount")) {
		t.Error("detector flagged a float64 count unmarshal")
	}
	parseSrc := `package p
import "encoding/json"
func f(result json.RawMessage) (int, error) {
	return parseJobID(result)
}
`
	if inlineJobIDUnmarshal(parseFuncDecl(t, parseSrc, "f")) {
		t.Error("detector flagged a function that only calls parseJobID")
	}
}
