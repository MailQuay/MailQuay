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
	p.AllowURLSchemes("cid")

	// Explicitly keep the image attributes the reader needs after src moves to
	// data-src.
	p.AllowAttrs("alt", "title").OnElements("img")
	p.AllowAttrs("width", "height").Matching(bluemonday.NumberOrPercent).OnElements("img")

	return p
}

var cssURL = regexp.MustCompile(`(?i)url\(\s*['"]?([^'")]+?)['"]?\s*\)`)

// SanitizeHTML applies an allowlist policy and then rewrites every resource URL
// into an inert data-src attribute. Links open in a new tab without leaking a
// referrer. The output never contains active content or remote loads.
func SanitizeHTML(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	clean := mailPolicy.Sanitize(raw)

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
			setAttribute(node, "data-src", src)
			removeAttribute(node, "src")
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
		if dataSrc, exists := attribute(node, "data-src"); !exists || dataSrc == "" {
			setAttribute(node, "data-src", background)
		}
		removeAttribute(node, "background")
	}

	if node.Data == "a" {
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
