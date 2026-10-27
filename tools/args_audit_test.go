package tools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file audits, by static analysis of the package source, that no tool
// handler reads an argument key its input schema does not declare. CallTool
// rejects undeclared keys (see validateArgs), so a key the handler reads but
// the schema omits would be unusable. A key the schema declares but the
// handler never reads is not flagged here: that is a documentation question,
// not a rejected-argument bug.
//
// What counts as reading a key:
//   - args["key"] on a map parameter named args or a;
//   - a helper call that passes args followed by a literal key, such as
//     getOptionalBool(args, "enable", true).
//
// Limitations: the audit only sees keys written as string literals. A key
// built from a const or var, from string concatenation, or read through a
// map under any other name (an alias such as params, or a copy passed to a
// helper that indexes it with a non-literal key) is invisible to it. It is
// a safety net against the common mistake, not a proof; the runtime check
// in CallTool is what actually enforces the schema.
//
// Reachability from a tool's Handler follows every identifier that names a
// package-level function, every method selected by a name that *Registry
// defines, and every method of a type that is mentioned (so &fooDryRun{}
// brings in its ExecuteDryRun). Interface dispatch through DryRunnable is
// therefore covered by the type mention in each handler's dry-run wrapper,
// not by method name.

// argAliases are the identifiers that hold a tool's argument map.
var argAliases = map[string]bool{"args": true, "a": true}

type toolSource struct {
	funcs   map[string]*ast.FuncDecl
	methods map[string]map[string]*ast.FuncDecl // receiver type -> method -> decl
	// handlers maps tool name to the handler's function name.
	handlers map[string]string
}

func loadToolSource(t *testing.T) *toolSource {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatal("no package source files found")
	}
	src := &toolSource{
		funcs:    map[string]*ast.FuncDecl{},
		methods:  map[string]map[string]*ast.FuncDecl{},
		handlers: map[string]string{},
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fd.Recv == nil {
				src.funcs[fd.Name.Name] = fd
				continue
			}
			typ := receiverTypeName(fd.Recv.List[0].Type)
			if src.methods[typ] == nil {
				src.methods[typ] = map[string]*ast.FuncDecl{}
			}
			src.methods[typ][fd.Name.Name] = fd
		}
	}
	reg := src.methods["Registry"]["registerTools"]
	if reg == nil {
		t.Fatal("Registry.registerTools not found")
	}
	ast.Inspect(reg.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		idx, ok := as.Lhs[0].(*ast.IndexExpr)
		if !ok {
			return true
		}
		sel, ok := idx.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "tools" {
			return true
		}
		name, ok := stringLit(idx.Index)
		if !ok {
			return true
		}
		lit, ok := as.Rhs[0].(*ast.CompositeLit)
		if !ok {
			t.Errorf("tool %s: registration is not a Tool literal", name)
			return true
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok || kv.Key.(*ast.Ident).Name != "Handler" {
				continue
			}
			switch h := kv.Value.(type) {
			case *ast.Ident:
				src.handlers[name] = h.Name
			case *ast.SelectorExpr:
				src.handlers[name] = h.Sel.Name
			default:
				t.Errorf("tool %s: Handler is not a plain function or method value; teach the audit about it", name)
			}
		}
		return true
	})
	return src
}

func receiverTypeName(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	return s, err == nil
}

func isArgAlias(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && argAliases[id.Name]
}

// keysRead returns the argument keys a tool's handler can read, with the
// functions that read them, following the reachability rules above.
func (s *toolSource) keysRead(handler string) map[string][]string {
	keys := map[string][]string{}
	seen := map[*ast.FuncDecl]bool{}
	var visit func(fd *ast.FuncDecl)
	visit = func(fd *ast.FuncDecl) {
		if fd == nil || seen[fd] || fd.Body == nil {
			return
		}
		seen[fd] = true
		addKey := func(k string) {
			keys[k] = append(keys[k], fd.Name.Name)
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.IndexExpr:
				if isArgAlias(n.X) {
					if k, ok := stringLit(n.Index); ok {
						addKey(k)
					}
				}
			case *ast.CallExpr:
				for i, arg := range n.Args {
					if isArgAlias(arg) && i+1 < len(n.Args) {
						if k, ok := stringLit(n.Args[i+1]); ok {
							addKey(k)
						}
					}
				}
			case *ast.Ident:
				visit(s.funcs[n.Name])
			case *ast.SelectorExpr:
				visit(s.methods["Registry"][n.Sel.Name])
			case *ast.CompositeLit:
				if id, ok := n.Type.(*ast.Ident); ok {
					for _, m := range s.methods[id.Name] {
						visit(m)
					}
				}
			}
			return true
		})
	}
	if fd := s.funcs[handler]; fd != nil {
		visit(fd)
	} else {
		visit(s.methods["Registry"][handler])
	}
	return keys
}

// knownSchemaMismatches lists tools whose handler reads keys the schema does
// not declare, with the exact set of undeclared keys for each. The audit must
// not hide new mismatches, so the set is compared exactly: a new undeclared
// key, or a key that was fixed, fails the test until this table is updated.
// Once a tool is fixed, remove its entry.
var knownSchemaMismatches = map[string][]string{}

func TestHandlersOnlyReadDeclaredArguments(t *testing.T) {
	src := loadToolSource(t)
	r := NewRegistry(nil, nil)

	if len(src.handlers) != len(r.tools) {
		t.Fatalf("audit found %d handlers for %d registered tools", len(src.handlers), len(r.tools))
	}
	var names []string
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		tool := r.tools[name]
		handler, ok := src.handlers[name]
		if !ok {
			t.Errorf("%s: handler not found by the audit", name)
			continue
		}
		read := src.keysRead(handler)
		var missing []string
		for key := range read {
			if !hasParam(tool, key) {
				missing = append(missing, key)
			}
		}
		sort.Strings(missing)
		if want, known := knownSchemaMismatches[name]; known {
			if len(missing) == 0 {
				t.Errorf("%s: no longer mismatched; remove it from knownSchemaMismatches", name)
			} else if strings.Join(missing, ",") != strings.Join(want, ",") {
				t.Errorf("%s: undeclared keys read by %s changed: got %v, want %v (update knownSchemaMismatches only if the change is intended)", name, handler, missing, want)
			}
			continue
		}
		if len(missing) > 0 {
			t.Errorf("%s: handler %s reads %v, which the input schema does not declare (add them to the schema in tools/registry.go or stop reading them)", name, handler, missing)
		}
	}
}

// TestAuditSeesKnownReads guards the audit itself: if it stops finding keys
// that are known to be read, a refactor has hidden handlers from it and the
// test above would pass vacuously.
func TestAuditSeesKnownReads(t *testing.T) {
	src := loadToolSource(t)
	for tool, want := range map[string][]string{
		"query_apps":     {"app_name", "include_config"},
		"install_app":    {"app_name", "dry_run"},
		"system_reboot":  {"delay", "dry_run"},
		"dismiss_alert":  {"uuid", "dry_run"},
		"create_dataset": {"name", "volblocksize"},
		"query_jobs":     {"limit"},
	} {
		read := src.keysRead(src.handlers[tool])
		for _, key := range want {
			if _, ok := read[key]; !ok {
				t.Errorf("audit found no read of %q by %s (found %v)", key, tool, read)
			}
		}
	}
}
