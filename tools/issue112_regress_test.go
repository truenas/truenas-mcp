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

// inlineJobIDUnmarshal reports whether fn still does `var jobID int` (or
// equivalent) and json.Unmarshal into that variable, instead of calling
// parseJobID.
func inlineJobIDUnmarshal(fn *ast.FuncDecl) bool {
	if fn.Body == nil {
		return false
	}
	declares := map[string]bool{}
	unmarshals := false
	callsParse := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ValueSpec:
			for _, ident := range x.Names {
				if ident.Name == "jobID" || ident.Name == "jobIDArray" || ident.Name == "jobIDs" {
					declares[ident.Name] = true
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && (id.Name == "jobID" || id.Name == "jobIDArray" || id.Name == "jobIDs") {
					if x.Tok == token.DEFINE {
						declares[id.Name] = true
					}
				}
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "json" && sel.Sel.Name == "Unmarshal" && len(x.Args) == 2 {
					if id, ok := x.Args[1].(*ast.UnaryExpr); ok {
						if v, ok := id.X.(*ast.Ident); ok && (v.Name == "jobID" || v.Name == "jobIDArray" || v.Name == "jobIDs") {
							unmarshals = true
						}
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
	return unmarshals && (declares["jobID"] || declares["jobIDArray"] || declares["jobIDs"])
}
