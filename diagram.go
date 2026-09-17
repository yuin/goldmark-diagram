// Package diagram is an extension for the goldmark(http://github.com/yuin/goldmark).
//
// This extension renders fenced code blocks written in diagram description
// languages (currently MermaidJS, rendered client-side or server-side via
// mmdc, and PlantUML, rendered server-side via plantuml) as diagrams instead
// of plain code blocks.
package diagram

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

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
		if cfg.umdURL != "" {
			rc.Set(mermaidUMDURLKey, cfg.umdURL)
		}
		_, _ = w.WriteString(`<pre class="mermaid">`)
		tw := html.ContextTextWriter(rc)
		_, _ = n.Value.WriteTo(tw, source)
		_, _ = w.WriteString("</pre>\n")
		return nil
	})
}

// }}} Mermaid client-side rendering

// Mermaid server-side rendering {{{

type mermaidServerRendererConfig struct {
	command string
	args    []string

	dual bool

	theme           string
	backgroundColor string

	lightTheme      string
	darkTheme       string
	lightBackground string
	darkBackground  string

	outputDir string
	urlPrefix string
}

// MermaidServerRendererOption is a functional option for [NewMermaidServerRenderer].
type MermaidServerRendererOption func(*mermaidServerRendererConfig)

// WithMermaidCommand sets the path to the `mmdc` (mermaid-cli) command.
//
// If not set, "mmdc" is resolved using the PATH environment variable.
func WithMermaidCommand(path string) MermaidServerRendererOption {
	return func(c *mermaidServerRendererConfig) {
		c.command = path
	}
}

// WithMermaidArgs sets additional arguments passed to the `mmdc` command,
// appended after the arguments generated internally (input/output/format/
// theme/background).
func WithMermaidArgs(args ...string) MermaidServerRendererOption {
	return func(c *mermaidServerRendererConfig) {
		c.args = args
	}
}

// WithMermaidTheme sets the MermaidJS theme used when dual theme rendering
// is not enabled via [WithMermaidDualTheme].
//
// It is ignored if [WithMermaidDualTheme] is used.
func WithMermaidTheme(theme string) MermaidServerRendererOption {
	return func(c *mermaidServerRendererConfig) {
		c.theme = theme
	}
}

// WithMermaidBackgroundColor sets the background color used when dual theme
// rendering is not enabled via [WithMermaidDualTheme].
//
// It is ignored if [WithMermaidDualTheme] is used.
func WithMermaidBackgroundColor(color string) MermaidServerRendererOption {
	return func(c *mermaidServerRendererConfig) {
		c.backgroundColor = color
	}
}

// WithMermaidDualTheme enables rendering the diagram twice, once with light
// and once with dark, and embeds both into a single <picture> element so the
// browser can switch between them based on `prefers-color-scheme`.
func WithMermaidDualTheme(light, dark string) MermaidServerRendererOption {
	return func(c *mermaidServerRendererConfig) {
		c.dual = true
		c.lightTheme = light
		c.darkTheme = dark
	}
}

// WithMermaidDualBackgroundColor sets the background colors used for the
// light and dark variants when [WithMermaidDualTheme] is used.
func WithMermaidDualBackgroundColor(light, dark string) MermaidServerRendererOption {
	return func(c *mermaidServerRendererConfig) {
		c.lightBackground = light
		c.darkBackground = dark
	}
}

// WithMermaidOutputDir makes [NewMermaidServerRenderer] write rendered SVGs
// as files under dir, instead of embedding them inline into the HTML output.
//
// Files are named after a hash of the diagram source, theme and background
// color, so identical diagrams are rendered once and reused.
func WithMermaidOutputDir(dir string) MermaidServerRendererOption {
	return func(c *mermaidServerRendererConfig) {
		c.outputDir = dir
	}
}

// WithMermaidURLPrefix sets the URL path prefix used when referencing files
// written to the directory configured via [WithMermaidOutputDir] (e.g. in
// `src`/`srcset` attributes).
//
// If not set, it defaults to "/" + the base name of the output directory.
func WithMermaidURLPrefix(prefix string) MermaidServerRendererOption {
	return func(c *mermaidServerRendererConfig) {
		c.urlPrefix = prefix
	}
}

type mermaidServerRenderer struct {
	cfg mermaidServerRendererConfig

	dirOnce sync.Once
	dirErr  error
}

// NewMermaidServerRenderer returns a [Renderer] that renders MermaidJS
// diagrams by invoking the `mmdc` (mermaid-cli) command as a subprocess.
//
// By default, the rendered SVG is embedded directly into the HTML output. If
// [WithMermaidOutputDir] is set, the SVG is written as a file under that
// directory and referenced via an `<img>` element instead. If
// [WithMermaidDualTheme] is set, the diagram is rendered twice (light and
// dark) and embedded using a `<picture>` element so the browser can switch
// between them based on `prefers-color-scheme`.
//
// If the command cannot be executed or exits with an error, an error message
// is rendered in a `<pre class="mermaid-error">` element instead of failing
// the whole document render.
func NewMermaidServerRenderer(opts ...MermaidServerRendererOption) Renderer {
	cfg := mermaidServerRendererConfig{
		command:         "mmdc",
		theme:           "default",
		backgroundColor: "white",
		lightTheme:      "default",
		darkTheme:       "dark",
		lightBackground: "white",
		darkBackground:  "transparent",
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.outputDir != "" && cfg.urlPrefix == "" {
		cfg.urlPrefix = defaultMermaidURLPrefix(cfg.outputDir)
	}
	return &mermaidServerRenderer{cfg: cfg}
}

type mermaidVariant struct {
	label      string // "", "light" or "dark"
	theme      string
	background string
}

type mermaidVariantResult struct {
	label string
	svg   []byte // used in inline mode
	url   string // used in output-dir mode
	err   error
}

func (r *mermaidServerRenderer) variants() []mermaidVariant {
	if r.cfg.dual {
		return []mermaidVariant{
			{label: "light", theme: r.cfg.lightTheme, background: r.cfg.lightBackground},
			{label: "dark", theme: r.cfg.darkTheme, background: r.cfg.darkBackground},
		}
	}
	return []mermaidVariant{
		{theme: r.cfg.theme, background: r.cfg.backgroundColor},
	}
}

// Render implements Renderer.Render.
func (r *mermaidServerRenderer) Render(w util.BufWriter, source []byte, n *ast.CodeBlock, rc renderer.Context) error {
	var buf bytes.Buffer
	_, _ = n.Value.WriteTo(&buf, source)
	src := buf.Bytes()

	variants := r.variants()
	results := make([]mermaidVariantResult, len(variants))
	for i, v := range variants {
		results[i] = r.renderVariant(v, src)
	}

	if err := combineMermaidErrors(results); err != nil {
		_, _ = w.WriteString(`<pre class="mermaid-error">`)
		tw := html.ContextTextWriter(rc)
		_, _ = tw.WriteString(err.Error())
		_, _ = w.WriteString("</pre>\n")
		return nil
	}

	r.writeOutput(w, results)
	return nil
}

func (r *mermaidServerRenderer) renderVariant(v mermaidVariant, src []byte) mermaidVariantResult {
	result := mermaidVariantResult{label: v.label}
	if r.cfg.outputDir == "" {
		svg, err := runMermaid(r.cfg.command, r.cfg.args, v.theme, v.background, src)
		result.svg, result.err = svg, err
		return result
	}
	url, err := r.renderToFile(v, src)
	result.url, result.err = url, err
	return result
}

func (r *mermaidServerRenderer) ensureDir() error {
	r.dirOnce.Do(func() {
		r.dirErr = os.MkdirAll(r.cfg.outputDir, 0o755)
	})
	return r.dirErr
}

func (r *mermaidServerRenderer) renderToFile(v mermaidVariant, src []byte) (string, error) {
	if err := r.ensureDir(); err != nil {
		return "", err
	}

	filename := mermaidCacheKey(src, v.theme, v.background) + ".svg"
	if v.label != "" {
		filename = mermaidCacheKey(src, v.theme, v.background) + "-" + v.label + ".svg"
	}

	target := filepath.Join(r.cfg.outputDir, filename)
	if info, err := os.Stat(target); err == nil && info.Size() > 0 {
		return mermaidFileURL(r.cfg.urlPrefix, filename), nil
	}

	svg, err := runMermaid(r.cfg.command, r.cfg.args, v.theme, v.background, src)
	if err != nil {
		return "", err
	}
	if err := writeMermaidFileAtomic(r.cfg.outputDir, filename, svg); err != nil {
		return "", err
	}
	return mermaidFileURL(r.cfg.urlPrefix, filename), nil
}

func (r *mermaidServerRenderer) writeOutput(w util.BufWriter, results []mermaidVariantResult) {
	if !r.cfg.dual {
		res := results[0]
		if r.cfg.outputDir == "" {
			_, _ = w.Write(res.svg)
			return
		}
		_, _ = w.WriteString(`<img src="`)
		_, _ = w.WriteString(res.url)
		_, _ = w.WriteString("\">\n")
		return
	}

	light, dark := mermaidResultByLabel(results, "light"), mermaidResultByLabel(results, "dark")
	lightSrc, darkSrc := light.url, dark.url
	if r.cfg.outputDir == "" {
		lightSrc = mermaidDataURI(light.svg)
		darkSrc = mermaidDataURI(dark.svg)
	}

	_, _ = w.WriteString("<picture>\n")
	_, _ = w.WriteString(`<source srcset="`)
	_, _ = w.WriteString(darkSrc)
	_, _ = w.WriteString(`" media="(prefers-color-scheme: dark)">` + "\n")
	_, _ = w.WriteString(`<img src="`)
	_, _ = w.WriteString(lightSrc)
	_, _ = w.WriteString("\">\n")
	_, _ = w.WriteString("</picture>\n")
}

func mermaidResultByLabel(results []mermaidVariantResult, label string) mermaidVariantResult {
	for _, res := range results {
		if res.label == label {
			return res
		}
	}
	return mermaidVariantResult{}
}

func mermaidDataURI(svg []byte) string {
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(svg)
}

func combineMermaidErrors(results []mermaidVariantResult) error {
	var msgs []string
	for _, res := range results {
		if res.err == nil {
			continue
		}
		if res.label == "" {
			msgs = append(msgs, res.err.Error())
			continue
		}
		msgs = append(msgs, fmt.Sprintf("%s: %s", res.label, res.err.Error()))
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}

func mermaidCacheKey(source []byte, theme, backgroundColor string) string {
	h := sha256.New()
	h.Write(source)
	h.Write([]byte{0})
	h.Write([]byte(theme))
	h.Write([]byte{0})
	h.Write([]byte(backgroundColor))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func writeMermaidFileAtomic(dir, filename string, data []byte) error {
	tmp, err := os.CreateTemp(dir, filename+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tmpPath)
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, filename)); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func mermaidFileURL(prefix, filename string) string {
	return path.Join(prefix, filename)
}

func defaultMermaidURLPrefix(outputDir string) string {
	base := filepath.Base(filepath.Clean(outputDir))
	if base == "." || base == "" || base == string(filepath.Separator) {
		return "/"
	}
	return "/" + base
}

func runMermaid(command string, extraArgs []string, theme, backgroundColor string, src []byte) ([]byte, error) {
	args := append([]string{"-i", "-", "-o", "-", "-e", "svg", "-t", theme, "-b", backgroundColor}, extraArgs...)
	cmd := exec.Command(command, args...) // nolint:gosec
	cmd.Stdin = bytes.NewReader(src)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := firstMermaidErrorMessage(stderr.String()); msg != "" {
			return nil, fmt.Errorf("mmdc: %s", msg)
		}
		return nil, fmt.Errorf("mmdc: %w", err)
	}
	return stdout.Bytes(), nil
}

func firstMermaidErrorMessage(stderr string) string {
	msg := strings.TrimSpace(stderr)
	if idx := strings.Index(msg, "\n\n"); idx >= 0 {
		msg = msg[:idx]
	}
	return strings.TrimSpace(msg)
}

// }}} Mermaid server-side rendering

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
