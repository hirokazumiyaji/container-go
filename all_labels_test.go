package container

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"testing"
)

// allLabels must set each internal label exactly once. A duplicated
// branch here is invisible to a map-equality assertion, because the
// second assignment writes the value the first one already wrote - so
// the runtime tests below pin the resulting label set (they catch a
// dropped or extra label) and the static check at the bottom is what
// actually catches a recurrence of the duplication.
func TestAllLabelsSetsEachInternalLabelOnce(t *testing.T) {
	cfg := newConfig()
	cfg.creation = "abc123"
	cfg.reuse = true
	cfg.reuseGroup = "grp"
	cfg.labels = map[string]string{"user": "value", managedLabel: "false"}

	got := cfg.allLabels()
	want := map[string]string{
		"user":          "value",
		managedLabel:    "true",
		sessionLabel:    sessionID(),
		creationLabel:   "abc123",
		reuseLabel:      "true",
		reuseGroupLabel: "grp",
	}
	if !maps.Equal(got, want) {
		t.Errorf("allLabels() = %v, want %v", got, want)
	}
}

// A config that activates no optional label must still carry the two
// labels that are unconditional, and no empty-valued ones.
func TestAllLabelsOmitsUnsetOptionalLabels(t *testing.T) {
	got := newConfig().allLabels()
	want := map[string]string{
		managedLabel: "true",
		sessionLabel: sessionID(),
	}
	if !maps.Equal(got, want) {
		t.Errorf("allLabels() = %v, want %v", got, want)
	}
}

// Internal labels win over a reserved key that reached config without
// going through WithLabels. WithLabels rejects those keys, so this
// state is only reachable by constructing a config directly - the
// overwrite is what keeps the generation label trustworthy if that
// ever stops being the only path.
func TestAllLabelsInternalLabelsWinOverReserved(t *testing.T) {
	cfg := newConfig()
	cfg.creation = "real"
	cfg.reuse = true
	cfg.reuseGroup = "realgroup"
	cfg.labels = map[string]string{
		managedLabel:    "false",
		sessionLabel:    "forged",
		creationLabel:   "forged",
		reuseLabel:      "false",
		reuseGroupLabel: "forged",
	}

	got := cfg.allLabels()
	want := map[string]string{
		managedLabel:    "true",
		sessionLabel:    sessionID(),
		creationLabel:   "real",
		reuseLabel:      "true",
		reuseGroupLabel: "realgroup",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("label %s = %q, want %q", k, got[k], v)
		}
	}
}

// TestAllLabelsWritesEachLabelOnce parses allLabels and counts the
// assignments to each reserved label key. Observing the returned map
// cannot do this: assigning the same key twice in the same block is
// the same observable result as assigning it once, so the duplicate
// that prompted this file survived every map-level assertion.
func TestAllLabelsWritesEachLabelOnce(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "options.go", nil, 0)
	if err != nil {
		t.Fatalf("parse options.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		f, ok := decl.(*ast.FuncDecl)
		if ok && f.Name.Name == "allLabels" {
			fn = f
			break
		}
	}
	if fn == nil {
		t.Fatal("allLabels not found in options.go")
	}

	writes := map[string]int{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range as.Lhs {
			idx, ok := lhs.(*ast.IndexExpr)
			if !ok {
				continue
			}
			key, ok := idx.Index.(*ast.Ident)
			if !ok {
				continue
			}
			// The source identifier is the constant's Go name, not
			// the label string it holds.
			switch key.Name {
			case "managedLabel", "sessionLabel", "creationLabel", "reuseLabel", "reuseGroupLabel":
				writes[key.Name]++
			}
		}
		return true
	})

	for _, name := range []string{"managedLabel", "sessionLabel", "creationLabel", "reuseLabel", "reuseGroupLabel"} {
		if writes[name] != 1 {
			t.Errorf("allLabels assigns %s %d times, want exactly 1", name, writes[name])
		}
	}
}
