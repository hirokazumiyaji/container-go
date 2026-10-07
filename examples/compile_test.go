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
	"strconv"
	"strings"
	"testing"
)

type documentationBlock struct {
	file     string
	line     int
	language string
	source   string
}

func TestDocumentationExamplesCompile(t *testing.T) {
	root := documentationRoot(t)
	docs := []string{
		filepath.Join(root, "README.md"),
		filepath.Join(root, "README.ja.md"),
		filepath.Join(root, "docs", "design.md"),
		filepath.Join(root, "docs", "design.ja.md"),
	}

	var blocks []documentationBlock
	for _, doc := range docs {
		docBlocks := readDocumentationBlocks(t, doc)
		hasGo := false
		for _, block := range docBlocks {
			if block.language == "go" {
				hasGo = true
				break
			}
		}
		if !hasGo {
			t.Fatalf("%s has no standalone Go examples", doc)
		}
		blocks = append(blocks, docBlocks...)
	}
	compileDocumentationBlocks(t, root, blocks)
}

func TestDocumentationExamplesHaveEnglishJapaneseParity(t *testing.T) {
	root := documentationRoot(t)
	pairs := [][2]string{
		{filepath.Join(root, "README.md"), filepath.Join(root, "README.ja.md")},
		{filepath.Join(root, "docs", "design.md"), filepath.Join(root, "docs", "design.ja.md")},
	}
	for _, pair := range pairs {
		english := readDocumentationBlocks(t, pair[0])
		japanese := readDocumentationBlocks(t, pair[1])
		if len(english) != len(japanese) {
			t.Fatalf("%s and %s have %d and %d fenced code blocks", pair[0], pair[1], len(english), len(japanese))
		}
		for i := range english {
			if english[i].language != japanese[i].language {
				t.Errorf("%s and %s code block %d use %q and %q", pair[0], pair[1], i+1, english[i].language, japanese[i].language)
				continue
			}
			want := normalizedDocumentationBlock(t, english[i])
			got := normalizedDocumentationBlock(t, japanese[i])
			if !bytes.Equal(want, got) {
				t.Errorf("%s and %s code block %d differ after comments are removed", pair[0], pair[1], i+1)
			}
		}
	}
}

func TestDocumentationModuleQuotesLocalReplacePath(t *testing.T) {
	root := filepath.Join("checkout with spaces", "container-go")
	got := documentationModule(root)
	want := `replace github.com/hirokazumiyaji/container-go => "` + filepath.ToSlash(root) + `"`
	if !strings.Contains(got, want) {
		t.Fatalf("temporary go.mod does not quote local replace path:\n%s", got)
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

func readDocumentationBlocks(t *testing.T, path string) []documentationBlock {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	var blocks []documentationBlock
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
		blocks = append(blocks, documentationBlock{
			file:     path,
			line:     startLine,
			language: language,
			source:   strings.Join(lines[start:i], "\n"),
		})
		inFence = false
	}
	if inFence {
		t.Fatalf("%s has an unclosed fenced code block", path)
	}
	return blocks
}

func normalizedDocumentationBlock(t *testing.T, block documentationBlock) []byte {
	t.Helper()
	if block.language == "go" {
		return normalizedGoSource(t, block)
	}
	return normalizedTextSource(block.source)
}

func normalizedGoSource(t *testing.T, block documentationBlock) []byte {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), block.file, block.source, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s:%d: %v", block.file, block.line, err)
	}
	file.Comments = nil
	var out bytes.Buffer
	if err := printer.Fprint(&out, token.NewFileSet(), file); err != nil {
		t.Fatalf("format %s:%d: %v", block.file, block.line, err)
	}
	return out.Bytes()
}

func normalizedTextSource(source string) []byte {
	var lines []string
	for _, line := range strings.Split(source, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		for _, marker := range []string{" //", "\t//", " #", "\t#"} {
			if i := strings.Index(line, marker); i >= 0 {
				line = strings.TrimSpace(line[:i])
				break
			}
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

func documentationModule(root string) string {
	extraRequires := ""
	if data, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		lines := strings.Split(string(data), "\n")
		var requires []string
		inRequire := false
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "require (") {
				inRequire = true
				continue
			}
			if inRequire {
				if trimmed == ")" {
					inRequire = false
				} else if trimmed != "" {
					requires = append(requires, "\t"+trimmed)
				}
				continue
			}
			if strings.HasPrefix(trimmed, "require ") && !strings.Contains(trimmed, "github.com/hirokazumiyaji/container-go") {
				requires = append(requires, "\t"+strings.TrimPrefix(trimmed, "require "))
			}
		}
		if len(requires) > 0 {
			extraRequires = "\n" + strings.Join(requires, "\n")
		}
	}
	return fmt.Sprintf("module documentation-example\n\ngo 1.23.0\n\nrequire (\n\tgithub.com/hirokazumiyaji/container-go v0.0.0%s\n)\n\nreplace github.com/hirokazumiyaji/container-go => %s\n", extraRequires, strconv.Quote(filepath.ToSlash(root)))
}

func compileDocumentationBlocks(t *testing.T, root string, blocks []documentationBlock) {
	t.Helper()
	dir := t.TempDir()
	module := documentationModule(root)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(module), 0o600); err != nil {
		t.Fatalf("write temporary go.mod: %v", err)
	}
	if sum, err := os.ReadFile(filepath.Join(root, "go.sum")); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o600); err != nil {
			t.Fatalf("write temporary go.sum: %v", err)
		}
	}

	compiled := 0
	for _, block := range blocks {
		if block.language != "go" {
			continue
		}
		if _, err := parser.ParseFile(token.NewFileSet(), block.file, block.source, 0); err != nil {
			t.Fatalf("parse %s:%d: %v", block.file, block.line, err)
		}
		compiled++
		snippetDir := filepath.Join(dir, fmt.Sprintf("snippet-%03d", compiled))
		if err := os.Mkdir(snippetDir, 0o700); err != nil {
			t.Fatalf("create temporary example directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(snippetDir, "example.go"), []byte(block.source), 0o600); err != nil {
			t.Fatalf("write temporary example: %v", err)
		}
	}
	if compiled == 0 {
		t.Fatal("documentation contains no Go blocks to compile")
	}

	cmd := exec.Command("go", "test", "-run", "^$", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compile documentation examples: %v\n%s", err, output)
	}
}
