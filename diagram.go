// Package diagram is an extension for the goldmark(http://github.com/yuin/goldmark).
//
// This extension renders fenced code blocks written in diagram description
// languages (currently MermaidJS and PlantUML) as diagrams instead of plain
// code blocks.
package diagram

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/renderer"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/util"
)

// Language is the identifier of a diagram description language, as it appears
// in the info string of a fenced code block (e.g. "mermaid" in ` ```mermaid `).
type Language string

// Supported diagram languages.
const (
	// LanguageMermaid is the [Language] for MermaidJS diagrams.
	LanguageMermaid Language = "mermaid"
	// LanguagePlantUML is the [Language] for PlantUML diagrams.
	LanguagePlantUML Language = "plantuml"
)

// Renderer renders the content of a diagram fenced code block as HTML.
//
// Implementations can rely on rc to communicate with other parts of the
// rendering (e.g. to ask the document-level decorator to emit extra markup
// such as a <script> tag once per document).
type Renderer interface {
	Render(w util.BufWriter, source []byte, n *ast.CodeBlock, rc renderer.Context) error
}

// RendererFunc adapts a function to a [Renderer].
type RendererFunc func(w util.BufWriter, source []byte, n *ast.CodeBlock, rc renderer.Context) error

// Render implements Renderer.Render.
func (f RendererFunc) Render(w util.BufWriter, source []byte, n *ast.CodeBlock, rc renderer.Context) error {
	return f(w, source, n, rc)
}

// Mermaid client-side rendering {{{

// DefaultMermaidModuleURL is the default URL that is used to load the
// MermaidJS ESM module for client-side rendering.
const DefaultMermaidModuleURL = "https://cdn.jsdelivr.net/npm/mermaid@latest/dist/mermaid.esm.min.mjs"

var mermaidModuleURLKey = renderer.NewContextKey()
var mermaidUMDURLKey = renderer.NewContextKey()

type mermaidClientRendererConfig struct {
	moduleURL string
	umdURL    string
}

// MermaidClientRendererOption is a functional option for [NewMermaidClientRenderer].
type MermaidClientRendererOption func(*mermaidClientRendererConfig)

// WithMermaidModuleURL sets the URL of the MermaidJS ESM module that is
// loaded via a <script type="module"> tag.
func WithMermaidModuleURL(url string) MermaidClientRendererOption {
	return func(c *mermaidClientRendererConfig) {
		c.moduleURL = url
	}
}

// WithMermaidUMDURL sets the URL of the MermaidJS UMD module that is
// loaded via a <script> tag for browsers that do not support ES modules.
//
// If this value is set, ModuleURL will be ignored.
func WithMermaidUMDURL(url string) MermaidClientRendererOption {
	return func(c *mermaidClientRendererConfig) {
		c.umdURL = url
	}
}

// NewMermaidClientRenderer returns a [Renderer] that renders MermaidJS diagrams
// using client-side rendering.
//
// It writes the diagram source into a `<pre class="mermaid">` element and marks
// the current [renderer.Context] so that the document-level decorator installed
// by [NewHTMLRenderer] emits a <script type="module"> tag that imports MermaidJS
// once, at the end of the document.
func NewMermaidClientRenderer(opts ...MermaidClientRendererOption) Renderer {
	cfg := mermaidClientRendererConfig{
		moduleURL: DefaultMermaidModuleURL,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return RendererFunc(func(w util.BufWriter, source []byte, n *ast.CodeBlock, rc renderer.Context) error {
		rc.Set(mermaidModuleURLKey, cfg.moduleURL)
		rc.Set(mermaidUMDURLKey, cfg.umdURL)
		_, _ = w.WriteString(`<pre class="mermaid">`)
		tw := html.ContextTextWriter(rc)
		_, _ = n.Value.WriteTo(tw, source)
		_, _ = w.WriteString("</pre>\n")
		return nil
	})
}

// }}} Mermaid client-side rendering

// PlantUML server-side rendering {{{

type plantUMLRendererConfig struct {
	command string
	args    []string
}

// PlantUMLRendererOption is a functional option for [NewPlantUMLRenderer].
type PlantUMLRendererOption func(*plantUMLRendererConfig)

// WithPlantUMLCommand sets the path to the `plantuml` command.
//
// If not set, "plantuml" is resolved using the PATH environment variable.
func WithPlantUMLCommand(path string) PlantUMLRendererOption {
	return func(c *plantUMLRendererConfig) {
		c.command = path
	}
}

// WithPlantUMLArgs sets the arguments passed to the `plantuml` command.
//
// The default is ["-tsvg", "-p", "-Djava.awt.headless=true"], which makes
// plantuml read a diagram from the standard input and write an SVG to the
// standard output.
func WithPlantUMLArgs(args ...string) PlantUMLRendererOption {
	return func(c *plantUMLRendererConfig) {
		c.args = args
	}
}

// NewPlantUMLRenderer returns a [Renderer] that renders PlantUML diagrams by
// invoking the `plantuml` command as a subprocess and embedding the resulting
// SVG directly into the output.
//
// If the command cannot be executed or exits with an error, an error message is
// rendered in a `<pre class="plantuml-error">` element instead of failing the
// whole document render.
func NewPlantUMLRenderer(opts ...PlantUMLRendererOption) Renderer {
	cfg := plantUMLRendererConfig{
		command: "plantuml",
		args:    []string{"-tsvg", "-p", "-Djava.awt.headless=true"},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return RendererFunc(func(w util.BufWriter, source []byte, n *ast.CodeBlock, rc renderer.Context) error {
		var buf bytes.Buffer
		_, _ = n.Value.WriteTo(&buf, source)
		svg, err := runPlantUML(cfg.command, cfg.args, buf.Bytes())
		if err != nil {
			_, _ = w.WriteString(`<pre class="plantuml-error">`)
			tw := html.ContextTextWriter(rc)
			_, _ = tw.WriteString(err.Error())
			_, _ = w.WriteString("</pre>\n")
			return nil
		}
		_, _ = w.Write(svg)
		return nil
	})
}

func runPlantUML(command string, args []string, src []byte) ([]byte, error) {
	cmd := exec.Command(command, args...) // nolint:gosec
	cmd.Stdin = bytes.NewReader(src)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("plantuml: %s", msg)
		}
		return nil, fmt.Errorf("plantuml: %w", err)
	}
	return stdout.Bytes(), nil
}

// }}} PlantUML server-side rendering

// HTML Renderer {{{

// ExcludeFunc is a function type that can be used to exclude certain code
// blocks from being rendered as diagrams, e.g. to let another extension
// render them instead.
type ExcludeFunc func(n *ast.CodeBlock, source []byte, rc renderer.Context) bool

type htmlRendererConfig struct {
	renderers   map[Language]Renderer
	excludeFunc ExcludeFunc
}

// HTMLRendererOption is a functional option for [NewHTMLRenderer].
type HTMLRendererOption func(*htmlRendererConfig)

// WithRenderer registers r as the [Renderer] used for fenced code blocks
// whose language is language, replacing any previously registered renderer
// for that language.
//
// This is the extension point that allows adding support for new diagram
// languages, or replacing the rendering strategy of an existing one (e.g.
// switching MermaidJS rendering from client-side to a future server-side
// implementation).
func WithRenderer(language Language, r Renderer) HTMLRendererOption {
	return func(c *htmlRendererConfig) {
		c.renderers[language] = r
	}
}

// WithExcludeFunc is a functional option that sets a function to exclude
// certain code blocks from being rendered as diagrams.
func WithExcludeFunc(f ExcludeFunc) HTMLRendererOption {
	return func(c *htmlRendererConfig) {
		c.excludeFunc = f
	}
}

// WithExcludeLanguages is a functional option that sets a list of languages
// to exclude from being rendered as diagrams.
func WithExcludeLanguages(langs ...Language) HTMLRendererOption {
	return func(c *htmlRendererConfig) {
		c.excludeFunc = func(n *ast.CodeBlock, source []byte, _ renderer.Context) bool {
			language, _ := n.Language(source)
			return slices.Contains(langs, Language(language))
		}
	}
}

type htmlRenderer struct {
	config htmlRendererConfig
}

// NewHTMLRenderer returns a new [html.Extension] that renders `mermaid` and
// `plantuml` fenced code blocks as diagrams.
func NewHTMLRenderer(opts ...HTMLRendererOption) html.Extension {
	cfg := htmlRendererConfig{
		renderers: map[Language]Renderer{
			LanguageMermaid:  NewMermaidClientRenderer(),
			LanguagePlantUML: NewPlantUMLRenderer(),
		},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &htmlRenderer{config: cfg}
}

// HTMLRenderer is a default [html.Extension] for diagrams.
var HTMLRenderer = NewHTMLRenderer()

func (r *htmlRenderer) RendererOptions(_ *html.Config) []html.Option {
	return []html.Option{
		html.WithNodeRendererDecorator(ast.KindCodeBlock, r.decorateCodeBlock),
		html.WithNodeRendererDecorator(ast.KindDocument, r.decorateDocument),
	}
}

func (r *htmlRenderer) decorateCodeBlock(next html.NodeRenderer) html.NodeRenderer {
	return html.NodeRendererFunc(func(w io.Writer, source []byte, node ast.Node,
		entering bool, rc renderer.Context) (ast.WalkStatus, error) {
		n := node.(*ast.CodeBlock)
		if n.CodeBlockKind != ast.CodeBlockKindFenced {
			return next.Render(w, source, n, entering, rc)
		}
		language, ok := n.Language(source)
		if !ok {
			return next.Render(w, source, n, entering, rc)
		}
		dr, ok := r.config.renderers[Language(language)]
		if !ok {
			return next.Render(w, source, n, entering, rc)
		}
		if r.config.excludeFunc != nil && r.config.excludeFunc(n, source, rc) {
			return next.Render(w, source, n, entering, rc)
		}
		if !entering {
			return ast.WalkContinue, nil
		}
		bw := w.(util.BufWriter)
		if err := dr.Render(bw, source, n, rc); err != nil {
			return ast.WalkStop, err
		}
		return ast.WalkContinue, nil
	})
}

func (r *htmlRenderer) decorateDocument(next html.NodeRenderer) html.NodeRenderer {
	return html.NodeRendererFunc(func(w io.Writer, source []byte, node ast.Node,
		entering bool, rc renderer.Context) (ast.WalkStatus, error) {
		if entering {
			return next.Render(w, source, node, entering, rc)
		}

		url, ok := rc.Get(mermaidUMDURLKey).(string)

		if ok {
			bw := w.(util.BufWriter)
			_, _ = bw.WriteString("<script src=\"")
			_, _ = bw.WriteString(url)
			_, _ = bw.WriteString("\"></script>\n")
			_, _ = bw.WriteString(`
<script>
  mermaid.initialize({ startOnLoad: true });
</script>`)
		} else {
			if url, ok := rc.Get(mermaidModuleURLKey).(string); ok {
				bw := w.(util.BufWriter)
				_, _ = bw.WriteString("<script type=\"module\">\nimport mermaid from '")
				_, _ = bw.WriteString(url)
				_, _ = bw.WriteString("';\n</script>\n")
			}
		}
		return next.Render(w, source, node, entering, rc)
	})
}

// }}} HTML Renderer
