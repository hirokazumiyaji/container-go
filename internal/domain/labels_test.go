package domain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"testing"
)

func TestBuildLabels(t *testing.T) {
	cfg := LabelConfig{
		Session:  "sess-123",
		Creation: "gen-456",
		Reuse:    true,
		Custom: map[string]string{
			"app": "web",
		},
	}

	labels := BuildLabels(cfg)
	if labels[LabelManaged] != "true" {
		t.Errorf("LabelManaged = %q, want true", labels[LabelManaged])
	}
	if labels[LabelSession] != "sess-123" {
		t.Errorf("LabelSession = %q, want sess-123", labels[LabelSession])
	}
	if labels[LabelCreation] != "gen-456" {
		t.Errorf("LabelCreation = %q, want gen-456", labels[LabelCreation])
	}
	if labels[LabelReuse] != "true" {
		t.Errorf("LabelReuse = %q, want true", labels[LabelReuse])
	}
	if labels["app"] != "web" {
		t.Errorf("custom label app = %q, want web", labels["app"])
	}
}

// The domain labels are the labels existing containers already carry.
// Changing this namespace would make reuse and cleanup stop recognizing
// them, so pin the values rather than only comparing constants.
func TestLabelNamespace(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"managed", LabelManaged, "com.github.hirokazumiyaji.container-go"},
		{"session", LabelSession, "com.github.hirokazumiyaji.container-go.session"},
		{"creation", LabelCreation, "com.github.hirokazumiyaji.container-go.creation"},
		{"reuse", LabelReuse, "com.github.hirokazumiyaji.container-go.reuse"},
		{"reuse-group", LabelReuseGroup, "com.github.hirokazumiyaji.container-go.reuse-group"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("label %s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

// Reserved keys in Custom must never forge ownership metadata, especially
// when the config did not request reuse.
func TestBuildLabelsDropsReservedCustomLabels(t *testing.T) {
	labels := BuildLabels(LabelConfig{
		Custom: map[string]string{
			LabelManaged:    "false",
			LabelSession:    "forged",
			LabelCreation:   "forged",
			LabelReuse:      "true",
			LabelReuseGroup: "forged",
			"app":           "web",
		},
	})
	want := map[string]string{
		LabelManaged: "true",
		"app":        "web",
	}
	if !maps.Equal(labels, want) {
		t.Errorf("BuildLabels() = %v, want %v", labels, want)
	}
}

// TestBuildLabelsWritesEachLabelOnce parses BuildLabels and counts the
// assignments to each reserved label key. Observing the returned map
// cannot catch a duplicate assignment, because assigning the same key
// twice produces the same result as assigning it once.
func TestBuildLabelsWritesEachLabelOnce(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "labels.go", nil, 0)
	if err != nil {
		t.Fatalf("parse labels.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		f, ok := decl.(*ast.FuncDecl)
		if ok && f.Name.Name == "BuildLabels" {
			fn = f
			break
		}
	}
	if fn == nil {
		t.Fatal("BuildLabels not found in labels.go")
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
			switch key.Name {
			case "LabelManaged", "LabelSession", "LabelCreation", "LabelReuse", "LabelReuseGroup":
				writes[key.Name]++
			}
		}
		return true
	})

	for _, name := range []string{"LabelManaged", "LabelSession", "LabelCreation", "LabelReuse", "LabelReuseGroup"} {
		if writes[name] != 1 {
			t.Errorf("BuildLabels assigns %s %d times, want exactly 1", name, writes[name])
		}
	}
}
