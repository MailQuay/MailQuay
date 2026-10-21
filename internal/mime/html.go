package mime

import (
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// mailStyles lists presentation properties that are safe and commonly emitted
// by mail clients. bluemonday validates each value with its default handler.
var mailStyles = []string{
	"background", "background-color", "background-image",
	"background-position", "background-repeat", "background-size",
	"border", "border-bottom", "border-bottom-color", "border-bottom-style",
	"border-bottom-width", "border-collapse", "border-color", "border-left",
	"border-left-color", "border-left-style", "border-left-width",
	"border-radius", "border-right", "border-right-color", "border-right-style",
	"border-right-width", "border-spacing", "border-style", "border-top",
	"border-top-color", "border-top-style", "border-top-width", "border-width",
	"caption-side", "clear", "color", "direction", "display", "empty-cells",
	"float", "font", "font-family", "font-size", "font-style", "font-variant",
	"font-weight", "height", "letter-spacing", "line-height", "list-style",
	"list-style-position", "list-style-type", "margin", "margin-bottom",
	"margin-left", "margin-right", "margin-top", "max-height", "max-width",
	"min-height", "min-width", "opacity", "overflow", "overflow-x",
	"overflow-y", "padding", "padding-bottom", "padding-left", "padding-right",
	"padding-top", "table-layout", "text-align", "text-decoration",
	"text-decoration-color", "text-decoration-line", "text-decoration-style",
	"text-indent", "text-transform", "vertical-align", "white-space", "width",
	"word-break", "word-spacing", "word-wrap",
}

var mailPolicy = buildMailPolicy()

func buildMailPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()

	// Mail relies on classes for client-specific styling and on a curated set
	// of inline CSS properties. style attributes are rewritten afterwards so
	// remote URLs never survive as loadable sources.
	p.AllowStyling()
	p.AllowAttrs("style").Globally()
	p.AllowStyles(mailStyles...).Globally()
	p.AllowURLSchemes("cid", "data", "http", "https")

	// Explicitly keep the image attributes the reader needs after src moves to
	// data-src.
	p.AllowAttrs("alt", "title").OnElements("img")
	p.AllowAttrs("width", "height").Matching(bluemonday.NumberOrPercent).OnElements("img")

	// Table layout attributes carry centering, widths and spacing in mail HTML.
	// Stripping them collapses otherwise valid templates.
	layoutElements := []string{"table", "thead", "tbody", "tfoot", "tr", "td", "th"}
	p.AllowAttrs("align").
		Matching(regexp.MustCompile(`(?i)^(left|center|right|justify)$`)).
		OnElements(append(append([]string{}, layoutElements...), "div", "p", "img")...)
	p.AllowAttrs("valign").
		Matching(regexp.MustCompile(`(?i)^(top|middle|bottom|baseline)$`)).
		OnElements(layoutElements...)
	p.AllowAttrs("width", "height").
		Matching(bluemonday.NumberOrPercent).
		OnElements(layoutElements...)
	p.AllowAttrs("cellpadding", "cellspacing", "border").
		Matching(regexp.MustCompile(`^[0-9]+$`)).
		OnElements("table")
	p.AllowAttrs("bgcolor").
		Matching(regexp.MustCompile(`(?i)^#[0-9a-f]{3,8}$|^[a-z]+$`)).
		OnElements(append(append([]string{}, layoutElements...), "body")...)
	p.AllowAttrs("background").OnElements("table", "td", "th", "body")

	return p
}

var (
	cssURL     = regexp.MustCompile(`(?i)url\(\s*['"]?([^'")]+?)['"]?\s*\)`)
	cssImport  = regexp.MustCompile(`(?is)@import[^;]*;`)
	cssUnsafe  = []string{"expression(", "javascript:", "vbscript:", "behavior:", "-moz-binding"}
	styleBlock = regexp.MustCompile(`(?is)<style[^>]*>([\s\S]*?)</style>`)
	rootStyle  = regexp.MustCompile(`(?is)<(?:html|body)\b[^>]*?style\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

// sanitizeCSS keeps layout rules from mail style blocks while removing remote
// loads and active CSS constructs. Remote backgrounds and imports would leak
// requests even when the reader has not allowed remote images.
// rootBackgroundCSS lifts background declarations from the html and body tags.
// Sanitizing a full document drops those tags, which would otherwise lose the
// page-level background color used by mail templates.
func rootBackgroundCSS(raw string) string {
	var declarations []string
	for _, match := range rootStyle.FindAllStringSubmatch(raw, -1) {
		style := match[1]
		if style == "" {
			style = match[2]
		}
		for _, declaration := range strings.Split(style, ";") {
			declaration = strings.TrimSpace(declaration)
			if strings.HasPrefix(strings.ToLower(declaration), "background") {
				declarations = append(declarations, declaration)
			}
		}
	}
	if len(declarations) == 0 {
		return ""
	}
	return "html, body { " + strings.Join(declarations, "; ") + " }"
}

func sanitizeCSS(css string) string {
	css = strings.NewReplacer("<", "", ">", "").Replace(css)
	css = cssImport.ReplaceAllString(css, "")
	css = cssURL.ReplaceAllStringFunc(css, func(match string) string {
		parts := cssURL.FindStringSubmatch(match)
		if len(parts) < 2 {
			return "none"
		}
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(parts[1])), "data:image/") {
			return match
		}
		return "none"
	})
	declarations := strings.Split(css, ";")
	kept := declarations[:0]
	for _, declaration := range declarations {
		lower := strings.ToLower(declaration)
		unsafe := false
		for _, token := range cssUnsafe {
			if strings.Contains(lower, token) {
				unsafe = true
				break
			}
		}
		if !unsafe {
			kept = append(kept, declaration)
		}
	}
	return strings.Join(kept, ";")
}

// SanitizeHTML applies an allowlist policy and then rewrites every resource URL
// into an inert data-src attribute. Links open in a new tab without leaking a
// referrer. The output never contains active content or remote loads.
func SanitizeHTML(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var styles []string
	if root := rootBackgroundCSS(raw); root != "" {
		styles = append(styles, sanitizeCSS(root))
	}
	raw = styleBlock.ReplaceAllStringFunc(raw, func(match string) string {
		parts := styleBlock.FindStringSubmatch(match)
		if len(parts) < 2 {
			return ""
		}
		if clean := strings.TrimSpace(sanitizeCSS(parts[1])); clean != "" {
			styles = append(styles, clean)
		}
		return ""
	})
	clean := mailPolicy.Sanitize(raw)
	if len(styles) > 0 {
		clean = "<style>" + strings.Join(styles, "\n") + "</style>" + clean
	}

	context := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(clean), context)
	if err != nil {
		// The input already passed the sanitizer; returning it unchanged is
		// safer than losing the message body.
		return clean
	}

	var out strings.Builder
	for _, node := range nodes {
		rewriteNode(node)
		if err := html.Render(&out, node); err != nil {
			return clean
		}
	}
	return out.String()
}

func rewriteNode(node *html.Node) {
	for child := node.FirstChild; child != nil; {
		next := child.NextSibling
		if child.Type == html.ElementNode && child.Data == "style" {
			for text := child.FirstChild; text != nil; text = text.NextSibling {
				if text.Type == html.TextNode {
					text.Data = sanitizeCSS(text.Data)
				}
			}
		}
		if child.Type == html.ElementNode && isDroppedElement(child.Data) {
			node.RemoveChild(child)
			child = next
			continue
		}
		rewriteNode(child)
		child = next
	}
	if node.Type == html.ElementNode {
		rewriteElement(node)
	}
}

func isDroppedElement(name string) bool {
	switch name {
	case "base", "link", "meta":
		return true
	}
	return false
}

func rewriteElement(node *html.Node) {
	if node.Data == "img" {
		if src, ok := attribute(node, "src"); ok {
			lower := strings.ToLower(strings.TrimSpace(src))
			if strings.HasPrefix(lower, "data:") && !strings.HasPrefix(lower, "data:image/") {
				removeAttribute(node, "src")
			} else {
				setAttribute(node, "data-src", src)
				removeAttribute(node, "src")
			}
		}
		removeAttribute(node, "srcset")
	}

	if style, ok := attribute(node, "style"); ok && style != "" {
		cleanStyle, urls := extractCSSURLs(style)
		if len(urls) > 0 {
			if dataSrc, exists := attribute(node, "data-src"); !exists || dataSrc == "" {
				setAttribute(node, "data-src", urls[0])
			}
		}
		if cleanStyle == "" {
			removeAttribute(node, "style")
		} else {
			setAttribute(node, "style", cleanStyle)
		}
	}

	if background, ok := attribute(node, "background"); ok && background != "" {
		setAttribute(node, "data-background-src", background)
		removeAttribute(node, "background")
	}

	if node.Data == "a" {
		if href, ok := attribute(node, "href"); ok && strings.HasPrefix(strings.ToLower(strings.TrimSpace(href)), "data:") {
			removeAttribute(node, "href")
		}
		setAttribute(node, "target", "_blank")
		setAttribute(node, "rel", "noopener noreferrer")
	}
}

func extractCSSURLs(style string) (string, []string) {
	var kept []string
	var urls []string
	for _, declaration := range strings.Split(style, ";") {
		declaration = strings.TrimSpace(declaration)
		if declaration == "" {
			continue
		}
		if matches := cssURL.FindStringSubmatch(declaration); len(matches) > 1 {
			if url := strings.TrimSpace(matches[1]); url != "" {
				urls = append(urls, url)
			}
			continue
		}
		kept = append(kept, declaration)
	}
	return strings.Join(kept, "; "), urls
}

func attribute(node *html.Node, name string) (string, bool) {
	for _, attr := range node.Attr {
		if attr.Key == name {
			return attr.Val, true
		}
	}
	return "", false
}

func setAttribute(node *html.Node, name, value string) {
	for i := range node.Attr {
		if node.Attr[i].Key == name {
			node.Attr[i].Val = value
			return
		}
	}
	node.Attr = append(node.Attr, html.Attribute{Key: name, Val: value})
}

func removeAttribute(node *html.Node, name string) {
	for i := range node.Attr {
		if node.Attr[i].Key == name {
			node.Attr = append(node.Attr[:i], node.Attr[i+1:]...)
			return
		}
	}
}

// textFromHTML produces readable plain text from sanitized HTML and is used
// when a message only carries an HTML body.
func textFromHTML(raw string) string {
	context := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(raw), context)
	if err != nil {
		return strings.TrimSpace(raw)
	}

	var out strings.Builder
	for _, node := range nodes {
		appendText(&out, node)
	}
	return normalizeText(out.String())
}

func appendText(out *strings.Builder, node *html.Node) {
	if node.Type == html.ElementNode {
		switch node.Data {
		case "script", "style", "head", "title", "noscript", "template":
			return
		case "br":
			out.WriteByte('\n')
			return
		}
		if isBlockElement(node.Data) {
			out.WriteByte('\n')
			appendChildrenText(out, node)
			out.WriteByte('\n')
			return
		}
	}
	appendChildrenText(out, node)
}

func appendChildrenText(out *strings.Builder, node *html.Node) {
	if node.Type == html.TextNode {
		out.WriteString(node.Data)
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		appendText(out, child)
	}
}

func isBlockElement(name string) bool {
	switch name {
	case "address", "article", "aside", "blockquote", "div", "dl", "fieldset",
		"figcaption", "figure", "footer", "form", "h1", "h2", "h3", "h4", "h5",
		"h6", "header", "hr", "li", "main", "nav", "ol", "p", "pre", "section",
		"table", "tbody", "td", "tfoot", "th", "thead", "tr", "ul":
		return true
	}
	return false
}

func normalizeText(text string) string {
	lines := strings.Split(text, "\n")
	cleaned := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if len(cleaned) == 0 || blank {
				continue
			}
			blank = true
			cleaned = append(cleaned, "")
			continue
		}
		blank = false
		cleaned = append(cleaned, line)
	}
	for len(cleaned) > 0 && cleaned[len(cleaned)-1] == "" {
		cleaned = cleaned[:len(cleaned)-1]
	}
	return strings.Join(cleaned, "\n")
}
