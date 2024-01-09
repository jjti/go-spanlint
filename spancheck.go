package spancheck

import (
	_ "embed"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"log"
	"regexp"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/ctrlflow"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/cfg"
)

//go:embed doc.go
var doc string

const stackLen = 32

var (
	// this approach stolen from errcheck
	// https://github.com/kisielk/errcheck/blob/7f94c385d0116ccc421fbb4709e4a484d98325ee/errcheck/errcheck.go#L22
	errorType = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)
)

// NewAnalyzerWithConfig returns a new analyzer configured with the Config passed in.
// Its config can be set for testing.
func NewAnalyzerWithConfig(config *Config) *analysis.Analyzer {
	return newAnalyzer(config)
}

func newAnalyzer(config *Config) *analysis.Analyzer {
	config.finalize()

	return &analysis.Analyzer{
		Name:  "spancheck",
		Doc:   extractDoc(doc, "spancheck"),
		Flags: config.fs,
		Run:   run(config),
		Requires: []*analysis.Analyzer{
			ctrlflow.Analyzer,
			inspect.Analyzer,
		},
	}
}

func run(config *Config) func(*analysis.Pass) (interface{}, error) {
	return func(pass *analysis.Pass) (interface{}, error) {
		inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

		nodeFilter := []ast.Node{
			(*ast.FuncLit)(nil),  // f := func() {}
			(*ast.FuncDecl)(nil), // func foo() {}
		}
		inspect.Preorder(nodeFilter, func(n ast.Node) {
			runFunc(pass, n, config)
		})

		return nil, nil
	}
}

type spanVar struct {
	stmt ast.Node
	id   *ast.Ident
	vr   *types.Var
}

// runFunc checks if the node is a function, has a span, and the span never has SetStatus set.
func runFunc(pass *analysis.Pass, node ast.Node, config *Config) {
	// copying https://cs.opensource.google/go/x/tools/+/master:go/analysis/passes/lostcancel/lostcancel.go

	// Find scope of function node
	var funcScope *types.Scope
	switch v := node.(type) {
	case *ast.FuncLit:
		funcScope = pass.TypesInfo.Scopes[v.Type]
	case *ast.FuncDecl:
		funcScope = pass.TypesInfo.Scopes[v.Type]
	}

	// Maps each span variable to its defining ValueSpec/AssignStmt.
	spanVars := make(map[*ast.Ident]spanVar)

	// Find the set of span vars to analyze.
	stack := make([]ast.Node, 0, stackLen)
	ast.Inspect(node, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.FuncLit:
			if len(stack) > 0 {
				return false // don't stray into nested functions
			}
		case nil:
			stack = stack[:len(stack)-1] // pop
			return true
		}
		stack = append(stack, n) // push

		// Look for [{AssignStmt,ValueSpec} CallExpr SelectorExpr]:
		//
		//   ctx, span     := otel.Tracer("app").Start(...)
		//   ctx, span     = otel.Tracer("app").Start(...)
		//   var ctx, span = otel.Tracer("app").Start(...)
		if !isTracerStart(pass.TypesInfo, n) || !isCall(stack[len(stack)-2]) {
			return true
		}

		stmt := stack[len(stack)-3]
		id := getID(stmt)
		if id == nil {
			pass.ReportRangef(n, "span is unassigned, probable memory leak")
			return true
		}

		if id.Name == "_" {
			pass.ReportRangef(id, "span is unassigned, probable memory leak")
		} else if v, ok := pass.TypesInfo.Uses[id].(*types.Var); ok {
			// If the span variable is defined outside function scope,
			// do not analyze it.
			if funcScope.Contains(v.Pos()) {
				spanVars[id] = spanVar{
					vr:   v,
					stmt: stmt,
					id:   id,
				}
			}
		} else if v, ok := pass.TypesInfo.Defs[id].(*types.Var); ok {
			spanVars[id] = spanVar{
				vr:   v,
				stmt: stmt,
				id:   id,
			}
		}

		return true
	})

	if len(spanVars) == 0 {
		return // no need to inspect CFG
	}

	// Obtain the CFG.
	cfgs := pass.ResultOf[ctrlflow.Analyzer].(*ctrlflow.CFGs)
	var g *cfg.CFG
	var sig *types.Signature
	switch node := node.(type) {
	case *ast.FuncDecl:
		sig, _ = pass.TypesInfo.Defs[node.Name].Type().(*types.Signature)
		g = cfgs.FuncDecl(node)
	case *ast.FuncLit:
		sig, _ = pass.TypesInfo.Types[node.Type].Type.(*types.Signature)
		g = cfgs.FuncLit(node)
	}
	if sig == nil {
		return // missing type information
	}

	// Check for missing calls.
	for _, sv := range spanVars {
		if config.endCheckEnabled {
			// Check if there's no End to the span.
			if ret := missingSpanCalls(pass, g, sv, "End", func(pass *analysis.Pass, ret *ast.ReturnStmt) *ast.ReturnStmt { return ret }, nil); ret != nil {
				pass.ReportRangef(sv.stmt, "%s.End is not called on all paths, possible memory leak", sv.vr.Name())
				pass.ReportRangef(ret, "return can be reached without calling %s.End", sv.vr.Name())
			}
		}

		if config.setStatusEnabled {
			// Check if there's no SetStatus to the span setting an error.
			if ret := missingSpanCalls(pass, g, sv, "SetStatus", returnsErr, config.ignoreChecksSignatures); ret != nil {
				pass.ReportRangef(sv.stmt, "%s.SetStatus is not called on all paths", sv.vr.Name())
				pass.ReportRangef(ret, "return can be reached without calling %s.SetStatus", sv.vr.Name())
			}
		}

		if config.recordErrorEnabled {
			// Check if there's no RecordError to the span setting an error.
			if ret := missingSpanCalls(pass, g, sv, "RecordError", returnsErr, config.ignoreChecksSignatures); ret != nil {
				pass.ReportRangef(sv.stmt, "%s.RecordError is not called on all paths", sv.vr.Name())
				pass.ReportRangef(ret, "return can be reached without calling %s.RecordError", sv.vr.Name())
			}
		}
	}
}

// isTracerStart reports whether n is tracer.Start()
func isTracerStart(info *types.Info, n ast.Node) bool {
	sel, ok := n.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	if sel.Sel.Name != "Start" {
		return false
	}

	obj, ok := info.Uses[sel.Sel]
	return ok && obj.Pkg().Path() == "go.opentelemetry.io/otel/trace"
}

func isCall(n ast.Node) bool {
	_, ok := n.(*ast.CallExpr)
	return ok
}

func getID(node ast.Node) *ast.Ident {
	switch stmt := node.(type) {
	case *ast.ValueSpec:
		if len(stmt.Names) > 1 {
			return stmt.Names[1]
		}
	case *ast.AssignStmt:
		if len(stmt.Lhs) > 1 {
			id, _ := stmt.Lhs[1].(*ast.Ident)
			return id
		}
	}
	return nil
}

// missingSpanCalls finds a path through the CFG, from stmt (which defines
// the 'span' variable v) to a return statement, that doesn't call the passed selector on the span.
func missingSpanCalls(
	pass *analysis.Pass,
	g *cfg.CFG,
	sv spanVar,
	selName string,
	checkErr func(pass *analysis.Pass, ret *ast.ReturnStmt) *ast.ReturnStmt,
	ignoreCheckSig *regexp.Regexp,
) *ast.ReturnStmt {
	// usesCall reports whether stmts contain a use of the selName call on variable v.
	usesCall := func(pass *analysis.Pass, stmts []ast.Node) bool {
		found, reAssigned := false, false
		for _, subStmt := range stmts {
			stack := []ast.Node{}
			ast.Inspect(subStmt, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.FuncLit:
					if len(stack) > 0 {
						return false // don't stray into nested functions
					}
				case *ast.CallExpr:
					if ident, ok := n.Fun.(*ast.Ident); ok {
						fnSig := pass.TypesInfo.ObjectOf(ident).String()
						if ignoreCheckSig != nil && ignoreCheckSig.MatchString(fnSig) {
							found = true
							return false
						}
					}
				case nil:
					stack = stack[:len(stack)-1] // pop
					return true
				}
				stack = append(stack, n) // push

				// Check whether the span was assigned over top of its old value.
				if isTracerStart(pass.TypesInfo, n) {
					if id := getID(stack[len(stack)-3]); id != nil && id.Obj.Decl == sv.id.Obj.Decl {
						reAssigned = true
						return false
					}
				}

				if n, ok := n.(*ast.SelectorExpr); ok {
					// Selector (End, SetStatus, RecordError) hit.
					if n.Sel.Name == selName {
						id, ok := n.X.(*ast.Ident)
						found = ok && id.Obj.Decl == sv.id.Obj.Decl
					}

					// Check if an ignore signature matches.
					fnSig := pass.TypesInfo.ObjectOf(n.Sel).String()
					if ignoreCheckSig != nil && ignoreCheckSig.MatchString(fnSig) {
						found = true
					}
				}

				return !found
			})
		}
		return found && !reAssigned
	}

	// blockUses computes "uses" for each block, caching the result.
	memo := make(map[*cfg.Block]bool)
	blockUses := func(pass *analysis.Pass, b *cfg.Block) bool {
		res, ok := memo[b]
		if !ok {
			res = usesCall(pass, b.Nodes)
			memo[b] = res
		}
		return res
	}

	// Find the var's defining block in the CFG,
	// plus the rest of the statements of that block.
	var defBlock *cfg.Block
	var rest []ast.Node
outer:
	for _, b := range g.Blocks {
		for i, n := range b.Nodes {
			if n == sv.stmt {
				defBlock = b
				rest = b.Nodes[i+1:]
				break outer
			}
		}
	}
	if defBlock == nil {
		log.Default().Print("[ERROR] internal error: can't find defining block for span var")
	}

	// Is the call "used" in the remainder of its defining block?
	if usesCall(pass, rest) {
		return nil
	}

	// Does the defining block return without making the call?
	if ret := defBlock.Return(); ret != nil {
		return checkErr(pass, ret)
	}

	// Search the CFG depth-first for a path, from defblock to a
	// return block, in which v is never "used".
	seen := make(map[*cfg.Block]bool)
	var search func(blocks []*cfg.Block) *ast.ReturnStmt
	search = func(blocks []*cfg.Block) *ast.ReturnStmt {
		for _, b := range blocks {
			if seen[b] {
				continue
			}
			seen[b] = true

			// Prune the search if the block uses v.
			if blockUses(pass, b) {
				continue
			}

			// Found path to return statement?
			if ret := returnsErr(pass, b.Return()); ret != nil {
				return ret // found
			}

			// Recur
			if ret := returnsErr(pass, search(b.Succs)); ret != nil {
				return ret
			}
		}
		return nil
	}

	return search(defBlock.Succs)
}

func returnsErr(pass *analysis.Pass, ret *ast.ReturnStmt) *ast.ReturnStmt {
	if ret == nil {
		return nil
	}

	for _, r := range ret.Results {
		if isErrorType(pass.TypesInfo.TypeOf(r)) {
			return ret
		}

		if r, ok := r.(*ast.CallExpr); ok {
			for _, err := range errorsByArg(pass, r) {
				if err {
					return ret
				}
			}
		}
	}

	return nil
}

// errorsByArg returns a slice s such that
// len(s) == number of return types of call
// s[i] == true iff return type at position i from left is an error type
//
// copied from https://github.com/kisielk/errcheck/blob/master/errcheck/errcheck.go
func errorsByArg(pass *analysis.Pass, call *ast.CallExpr) []bool {
	switch t := pass.TypesInfo.Types[call].Type.(type) {
	case *types.Named:
		// Single return
		return []bool{isErrorType(t)}
	case *types.Pointer:
		// Single return via pointer
		return []bool{isErrorType(t)}
	case *types.Tuple:
		// Multiple returns
		s := make([]bool, t.Len())
		for i := 0; i < t.Len(); i++ {
			switch et := t.At(i).Type().(type) {
			case *types.Named:
				// Single return
				s[i] = isErrorType(et)
			case *types.Pointer:
				// Single return via pointer
				s[i] = isErrorType(et)
			default:
				s[i] = false
			}
		}
		return s
	}
	return []bool{false}
}

func isErrorType(t types.Type) bool {
	return types.Implements(t, errorType)
}

// extractDoc extracts the doc comment for the analyzer with the given name.
// copied out of the internal go util: https://github.com/golang/tools/blob/master/internal/analysisinternal/extractdoc.go
func extractDoc(content, name string) string {
	if content == "" {
		log.Default().Print("[WARN] empty content")
		return ""
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", content, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		log.Default().Print("[WARN] failed to parse doc.go", err)
		return ""
	}

	if f.Doc == nil {
		log.Default().Print("[WARN] no package doc comment")
		return ""
	}

	for _, section := range strings.Split(f.Doc.Text(), "\n# ") {
		if body := strings.TrimPrefix(section, "Analyzer "+name); body != section &&
			body != "" &&
			body[0] == '\r' || body[0] == '\n' {
			body = strings.TrimSpace(body)
			rest := strings.TrimPrefix(body, name+":")
			if rest == body {
				log.Default().Print("[ERROR] package doc comment contains no '" + name + "' heading")
			}
			return strings.TrimSpace(rest)
		}
	}

	log.Default().Print("[ERROR] package doc comment contains no 'Analyzer " + name + "' heading")
	return ""
}
