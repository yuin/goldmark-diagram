package diagram_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	diagram "github.com/yuin/goldmark-diagram"
	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/util"
)

func convert(t *testing.T, r html.Renderer, source string) string {
	t.Helper()
	p := parser.New()
	node := p.Parse([]byte(source))
	var buf bytes.Buffer
	if err := r.Render(&buf, []byte(source), node); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestMermaidClientRendering(t *testing.T) {
	r := html.New(html.WithExtensions(diagram.HTMLRenderer))
	result := convert(t, r, "```mermaid\ngraph LR\n    A --- B\n```\n")

	if !strings.Contains(result, `<pre class="mermaid">graph LR`) {
		t.Fatalf("expected mermaid pre block, got:\n%s", result)
	}
	if !strings.Contains(result, "</pre>") {
		t.Fatalf("expected closing pre tag, got:\n%s", result)
	}
	if !strings.Contains(result, "<script type=\"module\">\nimport mermaid from '"+diagram.DefaultMermaidModuleURL+"';\n</script>") {
		t.Fatalf("expected mermaid module script tag, got:\n%s", result)
	}
}

func TestMermaidClientRenderingEscapesSource(t *testing.T) {
	r := html.New(html.WithExtensions(diagram.HTMLRenderer))
	result := convert(t, r, "```mermaid\nA --\"x\"--> B\n```\n")

	if !strings.Contains(result, "&quot;x&quot;") {
		t.Fatalf("expected escaped diagram source, got:\n%s", result)
	}
}

func TestMermaidScriptTagEmittedOnce(t *testing.T) {
	r := html.New(html.WithExtensions(diagram.HTMLRenderer))
	result := convert(t, r, "```mermaid\ngraph LR\n    A --> B\n```\n\n```mermaid\ngraph LR\n    C --> D\n```\n")

	if n := strings.Count(result, "<script type=\"module\">"); n != 1 {
		t.Fatalf("expected exactly one script tag, got %d in:\n%s", n, result)
	}
}

func TestMermaidCustomModuleURL(t *testing.T) {
	r := html.New(html.WithExtensions(diagram.NewHTMLRenderer(
		diagram.WithRenderer(diagram.LanguageMermaid, diagram.NewMermaidClientRenderer(
			diagram.WithMermaidModuleURL("https://example.com/mermaid.mjs"),
		)),
	)))
	result := convert(t, r, "```mermaid\ngraph LR\n    A --> B\n```\n")

	if !strings.Contains(result, "import mermaid from 'https://example.com/mermaid.mjs';") {
		t.Fatalf("expected custom module url, got:\n%s", result)
	}
}

func TestNonDiagramCodeBlockIsUnaffected(t *testing.T) {
	r := html.New(html.WithExtensions(diagram.HTMLRenderer))
	result := convert(t, r, "```go\nfunc main() {}\n```\n")

	expected := "<pre><code class=\"language-go\">func main() {}\n</code></pre>\n"
	if strings.TrimSpace(result) != strings.TrimSpace(expected) {
		t.Fatalf("got:\n%s\nexpected:\n%s", result, expected)
	}
}

func TestExcludeLanguages(t *testing.T) {
	r := html.New(html.WithExtensions(diagram.NewHTMLRenderer(
		diagram.WithExcludeLanguages(diagram.LanguageMermaid),
	)))
	result := convert(t, r, "```mermaid\ngraph LR\n    A --> B\n```\n")

	expected := "<pre><code class=\"language-mermaid\">graph LR\n    A --&gt; B\n</code></pre>\n"
	if strings.TrimSpace(result) != strings.TrimSpace(expected) {
		t.Fatalf("got:\n%s\nexpected:\n%s", result, expected)
	}
}

func TestExcludeFunc(t *testing.T) {
	r := html.New(html.WithExtensions(diagram.NewHTMLRenderer(
		diagram.WithExcludeFunc(func(n *ast.CodeBlock, source []byte, _ renderer.Context) bool {
			language, _ := n.Language(source)
			return diagram.Language(language) == diagram.LanguagePlantUML
		}),
	)))
	result := convert(t, r, "```plantuml\n@startuml\nA -> B\n@enduml\n```\n")

	if !strings.HasPrefix(strings.TrimSpace(result), "<pre><code class=\"language-plantuml\">") {
		t.Fatalf("expected excluded plantuml block to fall back to default rendering, got:\n%s", result)
	}
}

func TestCustomRenderer(t *testing.T) {
	r := html.New(html.WithExtensions(diagram.NewHTMLRenderer(
		diagram.WithRenderer("dot", diagram.RendererFunc(
			func(w util.BufWriter, _ []byte, _ *ast.CodeBlock, _ renderer.Context) error {
				_, _ = w.WriteString("<pre class=\"dot\">custom</pre>\n")
				return nil
			},
		)),
	)))
	result := convert(t, r, "```dot\ndigraph { a -> b }\n```\n")

	if !strings.Contains(result, `<pre class="dot">custom</pre>`) {
		t.Fatalf("expected custom renderer output, got:\n%s", result)
	}
}

func TestPlantUMLRendering(t *testing.T) {
	if _, err := exec.LookPath("plantuml"); err != nil {
		t.Skip("plantuml command is not available")
	}
	r := html.New(html.WithExtensions(diagram.HTMLRenderer))
	result := convert(t, r, "```plantuml\n@startuml\nHello <|-- World\n@enduml\n```\n")

	if !strings.Contains(result, "<svg") {
		t.Fatalf("expected svg output, got:\n%s", result)
	}
}

func TestPlantUMLCommandNotFound(t *testing.T) {
	r := html.New(html.WithExtensions(diagram.NewHTMLRenderer(
		diagram.WithRenderer(diagram.LanguagePlantUML, diagram.NewPlantUMLRenderer(
			diagram.WithPlantUMLCommand("goldmark-diagram-nonexistent-command"),
		)),
	)))
	result := convert(t, r, "```plantuml\n@startuml\nA -> B\n@enduml\n```\n")

	if !strings.Contains(result, `<pre class="plantuml-error">`) {
		t.Fatalf("expected plantuml error block, got:\n%s", result)
	}
}
