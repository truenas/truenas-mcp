package tools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// middlewareMethodContract lists every TrueNAS middleware method this
// server calls, and the TrueNAS releases each is expected on. The
// contract fails when a Call/CallWithTimeout/roundTrip/queryCount site
// uses a method that is not in this list, and when a listed method is
// not called anywhere.
//
// Versions are inclusive minima. pool.scrub.scrub is deprecated from
// 26.0 but is still the start method on every supported release.
var middlewareMethodContract = map[string]string{
	"alert.dismiss":                   "25.10",
	"alert.list":                      "25.10",
	"alert.restore":                   "25.10",
	"app.available":                   "25.10",
	"app.config":                      "25.10",
	"app.create":                      "25.10",
	"app.delete":                      "25.10",
	"app.query":                       "25.10",
	"app.start":                       "25.10",
	"app.stop":                        "25.10",
	"app.update":                      "25.10",
	"app.upgrade":                     "25.10",
	"app.upgrade_summary":             "25.10",
	"auth.login_ex":                   "25.10",
	"boot.environment.destroy":        "25.10",
	"boot.environment.query":          "25.10",
	"catalog.get_app_details":         "25.10",
	"certificate.query":               "25.10",
	"core.get_jobs":                   "25.10",
	"core.get_methods":                "25.10",
	"directoryservices.cache_refresh": "25.10",
	"directoryservices.config":        "25.10",
	"directoryservices.leave":         "25.10",
	"directoryservices.status":        "25.10",
	"directoryservices.update":        "25.10",
	"interface.query":                 "25.10",
	"pool.dataset.create":             "25.10",
	"pool.dataset.query":              "25.10",
	"pool.query":                      "25.10",
	"pool.scrub.create":               "25.10",
	"pool.scrub.delete":               "25.10",
	"pool.scrub.query":                "25.10",
	"pool.scrub.scrub":                "25.10",
	"pool.snapshot.query":             "25.10",
	"reporting.get_data":              "25.10",
	"reporting.graphs":                "25.10",
	"sharing.nfs.create":              "25.10",
	"sharing.nfs.query":               "25.10",
	"sharing.smb.create":              "25.10",
	"sharing.smb.query":               "25.10",
	"system.info":                     "25.10",
	"system.reboot":                   "25.10",
	"update.available_versions":       "25.10",
	"update.download":                 "25.10",
	"update.run":                      "25.10",
	"update.status":                   "25.10",
	"vm.query":                        "25.10",
}

// dispatchers are the functions that forward a method name to the
// middleware. A non-literal method argument is allowed only inside
// these functions (they take the method from their caller).
var methodDispatchers = map[string]bool{
	"Call":            true,
	"CallWithTimeout": true,
	"roundTrip":       true,
	"queryCount":      true,
}

// planExecutors pass plan.Method to Call. A non-literal method is allowed
// in their bodies; they are not themselves middleware dispatchers (their
// callers pass a *writePlan, not a method name).
var planExecutors = map[string]bool{
	"callPlan": true,
}

func TestMiddlewareMethodContract(t *testing.T) {
	root := repoRoot(t)
	found, problems := collectMiddlewareCalls(t, root)
	for _, p := range problems {
		t.Error(p)
	}
	for method := range found {
		if _, ok := middlewareMethodContract[method]; !ok {
			t.Errorf("middleware method %q is called but missing from middlewareMethodContract", method)
		}
	}
	for method := range middlewareMethodContract {
		if !found[method] {
			t.Errorf("middlewareMethodContract lists %q, which is not called", method)
		}
	}
}

func TestMiddlewareMethodContractFailsWhenACallIsMissing(t *testing.T) {
	src := `package p
func f(ctx context.Context, client interface{ Call(context.Context, string, ...interface{}) }) {
	client.Call(ctx, "pool.query")
	client.Call(ctx, "no.such.method")
}
`
	found, problems := callsInSource("fake.go", src)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if !found["no.such.method"] || !found["pool.query"] {
		t.Fatalf("found = %v", found)
	}
	if _, ok := middlewareMethodContract["no.such.method"]; ok {
		t.Fatal("no.such.method should not be in the live contract")
	}
	if _, ok := middlewareMethodContract["pool.query"]; !ok {
		t.Fatal("pool.query should be in the live contract")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), ".."))
}

func collectMiddlewareCalls(t *testing.T, root string) (map[string]bool, []string) {
	t.Helper()
	found := map[string]bool{}
	var problems []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "vendor" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		data, err := readFile(path)
		if err != nil {
			return err
		}
		got, probs := callsInSource(src, data)
		for m := range got {
			found[m] = true
		}
		planFound, planProbs := planMethodsInSource(src, data)
		for m := range planFound {
			found[m] = true
		}
		problems = append(problems, probs...)
		problems = append(problems, planProbs...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found, problems
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

func callsInSource(filename, src string) (map[string]bool, []string) {
	found := map[string]bool{}
	var problems []string
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, []string{filename + ": " + err.Error()}
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		inDispatcher := methodDispatchers[fn.Name.Name] || planExecutors[fn.Name.Name]
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := calleeName(call)
			if !methodDispatchers[name] {
				return true
			}
			method, literal, ok := methodArg(name, call.Args)
			if !ok {
				return true
			}
			if !literal {
				if inDispatcher {
					return true
				}
				pos := fset.Position(call.Pos())
				problems = append(problems, pos.String()+": "+name+" uses a non-literal method name; add it to a dispatcher or pass a string literal")
				return true
			}
			found[method] = true
			return true
		})
	}
	return found, problems
}

func calleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func methodArg(fn string, args []ast.Expr) (method string, literal, ok bool) {
	if len(args) == 0 {
		return "", false, false
	}
	// All dispatchers now take context.Context first:
	// Call(ctx, method, ...), CallWithTimeout(ctx, timeout, method, ...),
	// roundTrip(ctx, conn, method, ...), queryCount(ctx, client, method, ...).
	idx := 1
	if fn == "CallWithTimeout" || fn == "roundTrip" || fn == "queryCount" {
		idx = 2
	}
	if len(args) <= idx {
		return "", false, false
	}
	switch a := args[idx].(type) {
	case *ast.BasicLit:
		if a.Kind != token.STRING {
			return "", false, true
		}
		s, err := strconv.Unquote(a.Value)
		if err != nil {
			return "", false, true
		}
		return s, true, true
	default:
		return "", false, true
	}
}

// planMethodsInSource records middleware methods assigned to writePlan.Method
// from a *Method constant. A raw string literal (or any non-*Method
// expression) is a contract problem. Unused *Method constants are not
// counted as called.
func planMethodsInSource(filename, src string) (map[string]bool, []string) {
	found := map[string]bool{}
	var problems []string
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return found, []string{filename + ": " + err.Error()}
	}
	consts := map[string]string{}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasSuffix(name.Name, "Method") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil || !strings.Contains(s, ".") {
					continue
				}
				consts[name.Name] = s
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isWritePlanComposite(lit) {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "Method" {
				continue
			}
			pos := fset.Position(kv.Value.Pos())
			ident, ok := kv.Value.(*ast.Ident)
			if !ok || !strings.HasSuffix(ident.Name, "Method") {
				problems = append(problems, pos.String()+": writePlan.Method must be a *Method constant, not a raw literal or other expression")
				continue
			}
			method, ok := consts[ident.Name]
			if !ok {
				problems = append(problems, pos.String()+": writePlan.Method uses "+ident.Name+", which is not a *Method string constant in this file")
				continue
			}
			found[method] = true
		}
		return true
	})
	return found, problems
}

func isWritePlanComposite(lit *ast.CompositeLit) bool {
	switch t := lit.Type.(type) {
	case *ast.Ident:
		return t.Name == "writePlan"
	}
	return false
}

func TestPlanMethodContractRequiresNamedConstants(t *testing.T) {
	src := `package p
const usedMethod = "app.start"
const unusedMethod = "foo.bar"
func f() *writePlan {
	return &writePlan{Method: usedMethod}
}
`
	found, problems := planMethodsInSource("fake.go", src)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if !found["app.start"] {
		t.Fatalf("found = %v, want app.start from usedMethod", found)
	}
	if found["foo.bar"] {
		t.Fatalf("unused *Method constant was counted as called: %v", found)
	}
}

func TestPlanMethodContractRejectsRawLiteral(t *testing.T) {
	src := `package p
func f() *writePlan {
	return &writePlan{Method: "app.stop"}
}
`
	_, problems := planMethodsInSource("fake.go", src)
	if len(problems) == 0 {
		t.Fatal("want a problem for a raw writePlan.Method literal")
	}
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "*Method constant") {
		t.Errorf("problem should mention a *Method constant, got %q", joined)
	}
}
