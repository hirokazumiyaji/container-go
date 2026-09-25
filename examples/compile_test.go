package examples

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type documentationSnippet struct {
	file   string
	line   int
	source string
}

func TestDocumentationExamplesCompile(t *testing.T) {
	root := documentationRoot(t)
	docs := []string{
		filepath.Join(root, "README.md"),
		filepath.Join(root, "README.ja.md"),
		filepath.Join(root, "docs", "design.md"),
		filepath.Join(root, "docs", "design.ja.md"),
	}

	var snippets []documentationSnippet
	for _, doc := range docs {
		docSnippets := readDocumentationSnippets(t, doc)
		if len(docSnippets) == 0 {
			t.Fatalf("%s has no standalone Go examples", doc)
		}
		snippets = append(snippets, docSnippets...)
	}
	compileDocumentationSnippets(t, root, snippets)
}

func TestDocumentationExamplesHaveEnglishJapaneseParity(t *testing.T) {
	root := documentationRoot(t)
	pairs := [][2]string{
		{filepath.Join(root, "README.md"), filepath.Join(root, "README.ja.md")},
		{filepath.Join(root, "docs", "design.md"), filepath.Join(root, "docs", "design.ja.md")},
	}
	for _, pair := range pairs {
		english := readDocumentationSnippets(t, pair[0])
		japanese := readDocumentationSnippets(t, pair[1])
		if len(english) != len(japanese) {
			t.Fatalf("%s and %s have %d and %d Go examples", pair[0], pair[1], len(english), len(japanese))
		}
		for i := range english {
			want := normalizedGoSource(t, english[i])
			got := normalizedGoSource(t, japanese[i])
			if !bytes.Equal(want, got) {
				t.Errorf("%s and %s Go example %d differ after comments are removed", pair[0], pair[1], i+1)
			}
		}
	}
}

func documentationRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), ".."))
}

func readDocumentationSnippets(t *testing.T, path string) []documentationSnippet {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	var snippets []documentationSnippet
	inFence := false
	language := ""
	startLine := 0
	start := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "```") {
			continue
		}
		if !inFence {
			inFence = true
			language = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			startLine = i + 2
			start = i + 1
			continue
		}
		if language == "go" {
			snippets = append(snippets, documentationSnippet{
				file:   path,
				line:   startLine,
				source: strings.Join(lines[start:i], "\n"),
			})
		}
		inFence = false
	}
	if inFence {
		t.Fatalf("%s has an unclosed fenced code block", path)
	}
	return snippets
}

func normalizedGoSource(t *testing.T, snippet documentationSnippet) []byte {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), snippet.file, snippet.source, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s:%d: %v", snippet.file, snippet.line, err)
	}
	file.Comments = nil
	var out bytes.Buffer
	if err := printer.Fprint(&out, token.NewFileSet(), file); err != nil {
		t.Fatalf("format %s:%d: %v", snippet.file, snippet.line, err)
	}
	return out.Bytes()
}

func compileDocumentationSnippets(t *testing.T, root string, snippets []documentationSnippet) {
	t.Helper()
	dir := t.TempDir()
	module := "module documentation-example\n\ngo 1.23.0\n\nrequire github.com/hirokazumiyaji/container-go v0.0.0\n\nreplace github.com/hirokazumiyaji/container-go => " + filepath.ToSlash(root) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(module), 0o600); err != nil {
		t.Fatalf("write temporary go.mod: %v", err)
	}
	for i, snippet := range snippets {
		if _, err := parser.ParseFile(token.NewFileSet(), snippet.file, snippet.source, 0); err != nil {
			t.Fatalf("parse %s:%d: %v", snippet.file, snippet.line, err)
		}
		snippetDir := filepath.Join(dir, fmt.Sprintf("snippet-%03d", i+1))
		if err := os.Mkdir(snippetDir, 0o700); err != nil {
			t.Fatalf("create temporary example directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(snippetDir, "example.go"), []byte(snippet.source), 0o600); err != nil {
			t.Fatalf("write temporary example: %v", err)
		}
	}

	cmd := exec.Command("go", "test", "-run", "^$", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compile documentation examples: %v\n%s", err, output)
	}
}
