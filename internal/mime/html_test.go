package mime_test

import (
	"strings"
	"testing"

	mailmime "github.com/mailquay/mailquay/internal/mime"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

func parseFragment(t *testing.T, raw string) []*html.Node {
	t.Helper()
	context := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
	nodes, err := html.ParseFragment(strings.NewReader(raw), context)
	if err != nil {
		t.Fatalf("parse sanitized HTML: %v", err)
	}
	return nodes
}

func findElement(nodes []*html.Node, name string) *html.Node {
	var found *html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if found != nil {
			return
		}
		if node.Type == html.ElementNode && node.Data == name {
			found = node
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	for _, node := range nodes {
		walk(node)
	}
	return found
}

func attrs(node *html.Node) map[string]string {
	out := make(map[string]string, len(node.Attr))
	for _, attr := range node.Attr {
		out[attr.Key] = attr.Val
	}
	return out
}

func TestSanitizeHTMLRemovesActiveContent(t *testing.T) {
	raw := `<html><head><base href="https://evil.example/">` +
		`<meta http-equiv="refresh" content="0;url=https://evil.example/">` +
		`<link rel="stylesheet" href="https://evil.example/x.css"></head>` +
		`<body onload="alert(1)">` +
		`<script>alert(2)</script>` +
		`<iframe src="https://evil.example/"></iframe>` +
		`<object data="x"></object><embed src="x">` +
		`<form action="https://evil.example/"><input name="x"></form>` +
		`<p onclick="alert(3)">text</p>` +
		`</body></html>`

	out := mailmime.SanitizeHTML(raw)
	for _, forbidden := range []string{
		"<script", "<iframe", "<object", "<embed", "<form", "<input",
		"<base", "<meta", "<link", "onload", "onclick",
	} {
		if strings.Contains(strings.ToLower(out), forbidden) {
			t.Errorf("sanitized output contains %q: %s", forbidden, out)
		}
	}
	if !strings.Contains(out, "text") {
		t.Errorf("safe text was dropped: %s", out)
	}
}

func TestSanitizeHTMLKeepsInlineDataImages(t *testing.T) {
	raw := `<img src="data:image/png;base64,AAAA" alt="logo">` +
		`<a href="data:text/html,<b>x</b>">bad</a>` +
		`<img src="data:text/plain;base64,QQ==">`

	out := mailmime.SanitizeHTML(raw)
	if !strings.Contains(out, `data-src="data:image/png;base64,AAAA"`) {
		t.Errorf("inline data image lost: %s", out)
	}
	if strings.Contains(out, "data:text/plain") {
		t.Errorf("non-image data URL kept: %s", out)
	}
	if strings.Contains(out, `href="data:`) {
		t.Errorf("data link kept: %s", out)
	}
}

func TestSanitizeHTMLKeepsTableLayoutAttributes(t *testing.T) {
	raw := `<table align="center" width="600" cellpadding="0" cellspacing="0" border="0">` +
		`<tr><td align="right" valign="top" width="50%" bgcolor="#ffffff">x</td></tr></table>`

	out := mailmime.SanitizeHTML(raw)
	for _, want := range []string{
		`align="center"`, `width="600"`, `cellpadding="0"`, `cellspacing="0"`, `border="0"`,
		`align="right"`, `valign="top"`, `width="50%"`, `bgcolor="#ffffff"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("layout attribute %q lost: %s", want, out)
		}
	}
}

func TestSanitizeHTMLLiftsRootBackground(t *testing.T) {
	raw := `<html style="background: url(https://tracker.example/bg.png) repeat; background-color: #5c65ae">` +
		`<body style="background-color: #ffffff"><p>x</p></body></html>`

	out := mailmime.SanitizeHTML(raw)
	if !strings.Contains(out, "html, body {") || !strings.Contains(out, "#5c65ae") {
		t.Errorf("root background lost: %s", out)
	}
	if strings.Contains(out, "tracker.example") {
		t.Errorf("remote root background kept: %s", out)
	}
}

func TestSanitizeHTMLKeepsSafeStyleBlocks(t *testing.T) {
	raw := `<style>@media only screen { .container { width: 95% !important } } .vmiss-logo { width: 150px }</style>` +
		`<style>.x { background: url(https://tracker.example/a.png); color: #111111 }</style>` +
		`<p class="container">text</p>`

	out := mailmime.SanitizeHTML(raw)
	for _, want := range []string{"@media only screen", ".vmiss-logo", "width: 150px"} {
		if !strings.Contains(out, want) {
			t.Errorf("style block lost %q: %s", want, out)
		}
	}
	if strings.Contains(out, "tracker.example") {
		t.Errorf("remote CSS URL kept: %s", out)
	}
}

func TestSanitizeHTMLRewritesRemoteResources(t *testing.T) {
	raw := `<p class="mail-para" style="color: red">Hi ` +
		`<a href="https://example.com/path">link</a></p>` +
		`<div class="banner" style="background-image: url('https://tracker.example/bg.png'); color: blue">bg</div>` +
		`<table background="https://tracker.example/table.png"><tr><td>x</td></tr></table>` +
		`<img src="https://tracker.example/pixel.gif" alt="pixel" width="1" height="1" onerror="x">`

	out := mailmime.SanitizeHTML(raw)
	nodes := parseFragment(t, out)

	link := findElement(nodes, "a")
	if link == nil {
		t.Fatalf("anchor missing: %s", out)
	}
	linkAttrs := attrs(link)
	if linkAttrs["href"] != "https://example.com/path" {
		t.Errorf("href = %q", linkAttrs["href"])
	}
	if linkAttrs["target"] != "_blank" || linkAttrs["rel"] != "noopener noreferrer" {
		t.Errorf("link attributes = %+v", linkAttrs)
	}

	img := findElement(nodes, "img")
	if img == nil {
		t.Fatalf("image missing: %s", out)
	}
	imgAttrs := attrs(img)
	if imgAttrs["data-src"] != "https://tracker.example/pixel.gif" {
		t.Errorf("data-src = %q", imgAttrs["data-src"])
	}
	if _, ok := imgAttrs["src"]; ok {
		t.Errorf("img still has src: %+v", imgAttrs)
	}
	if imgAttrs["alt"] != "pixel" || imgAttrs["width"] != "1" || imgAttrs["height"] != "1" {
		t.Errorf("img metadata lost: %+v", imgAttrs)
	}

	table := findElement(nodes, "table")
	if table == nil || attrs(table)["data-background-src"] != "https://tracker.example/table.png" {
		t.Errorf("table background not rewritten: %+v", table)
	}
	if _, ok := imgAttrs["onerror"]; ok {
		t.Errorf("event handler survived: %+v", imgAttrs)
	}

	div := findElement(nodes, "div")
	if div == nil {
		t.Fatalf("div missing: %s", out)
	}
	divAttrs := attrs(div)
	if divAttrs["data-src"] != "https://tracker.example/bg.png" {
		t.Errorf("background data-src = %q", divAttrs["data-src"])
	}
	if strings.Contains(strings.ToLower(divAttrs["style"]), "url(") {
		t.Errorf("style still contains url(): %q", divAttrs["style"])
	}
	if !strings.Contains(divAttrs["style"], "color: blue") {
		t.Errorf("safe style lost: %q", divAttrs["style"])
	}
	if divAttrs["class"] != "banner" {
		t.Errorf("class lost: %+v", divAttrs)
	}
}

func TestSanitizeHTMLPrefersImageSource(t *testing.T) {
	out := mailmime.SanitizeHTML(`<img src="https://cdn.example/p.png" style="background-image: url('https://tracker.example/bg.png')">`)
	img := findElement(parseFragment(t, out), "img")
	if img == nil {
		t.Fatalf("image missing: %s", out)
	}
	got := attrs(img)
	if got["data-src"] != "https://cdn.example/p.png" {
		t.Errorf("data-src = %q", got["data-src"])
	}
	if strings.Contains(strings.ToLower(got["style"]), "url(") {
		t.Errorf("style still contains url(): %q", got["style"])
	}
}

func TestSanitizeHTMLPreservesCIDImages(t *testing.T) {
	out := mailmime.SanitizeHTML(`<img src="cid:part1.abc@example.com" alt="inline">`)
	if !strings.Contains(out, `data-src="cid:part1.abc@example.com"`) {
		t.Errorf("cid image not preserved: %s", out)
	}
}

func TestSanitizeHTMLEmpty(t *testing.T) {
	if got := mailmime.SanitizeHTML("   "); got != "" {
		t.Errorf("SanitizeHTML(blank) = %q", got)
	}
}
