// Package relnotes turns a CHANGELOG.md into the release-notes page of our
// apps: a parsed list of releases, an HTML fragment, a JSON handler, a
// standalone page for single-page tools, and a check that the running VERSION
// has an entry.
//
// Canonical source: https://gitlab.gutocarvalho.net/open/gcn/relnotes
// Apps carry a copy in internal/relnotes (no network at build time); update it
// with the sync script there, never by hand.
//
// CHANGELOG.md format (newest first):
//
//	# Changelog                      <- anything before the first "## " is ignored
//
//	## 1.2.0 — 2026-11-06           <- "## [v]X.Y.Z [-|—|–] YYYY-MM-DD"
//
//	Optional summary paragraph.
//
//	### Added                        <- optional groups
//	- A bullet. `code`, **bold** and [links](https://…) are kept.
//	  Indented lines continue the bullet.
//
// A "## Unreleased" section is skipped.
package relnotes

import (
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"regexp"
	"strings"
)

// Version of this package. Bump on every change, so a copy can be traced.
const Version = "1.0.0"

// Item is one bullet: its markdown source and the rendered HTML.
type Item struct {
	Text string        `json:"text"`
	HTML template.HTML `json:"-"`
}

// Group is a "### Title" block inside a release; Title is empty for bullets
// that come before any heading.
type Group struct {
	Title string `json:"title,omitempty"`
	Items []Item `json:"items"`
}

// Release is one "## version" section.
type Release struct {
	Version string        `json:"version"`
	Date    string        `json:"date,omitempty"`
	Summary string        `json:"summary,omitempty"`
	HTML    template.HTML `json:"-"` // rendered summary
	Groups  []Group       `json:"groups"`
}

// Notes returns every item of the release, ignoring groups.
func (r Release) Notes() []Item {
	var out []Item
	for _, g := range r.Groups {
		out = append(out, g.Items...)
	}
	return out
}

var (
	headRe   = regexp.MustCompile(`^##\s+\[?v?([0-9][0-9A-Za-z.+-]*?)\]?(?:\s*[-—–:]?\s*\(?(\d{4}-\d{2}-\d{2})\)?)?(?:\s.*)?$`)
	bulletRe = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
)

// Parse reads a CHANGELOG.md. It fails if a "## " heading is not a version or
// if no release is found, so a broken file is caught by the app's tests.
func Parse(md string) ([]Release, error) {
	var (
		out     []Release
		cur     *Release
		item    *Item
		summary []string
		skip    bool
	)
	flushSummary := func() {
		if cur != nil && len(summary) > 0 {
			s := strings.Join(summary, " ")
			if cur.Summary != "" {
				cur.Summary += " "
			}
			cur.Summary += s
		}
		summary = nil
	}
	group := func() *Group {
		if len(cur.Groups) == 0 {
			cur.Groups = append(cur.Groups, Group{})
		}
		return &cur.Groups[len(cur.Groups)-1]
	}
	for n, line := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "## "):
			flushSummary()
			item = nil
			if strings.EqualFold(strings.Trim(strings.TrimSpace(trim[3:]), "[]"), "unreleased") {
				skip, cur = true, nil
				continue
			}
			m := headRe.FindStringSubmatch(trim)
			if m == nil {
				return nil, fmt.Errorf("line %d: %q is not a version heading", n+1, trim)
			}
			skip = false
			out = append(out, Release{Version: m[1], Date: m[2]})
			cur = &out[len(out)-1]
		case skip || cur == nil:
			continue
		case strings.HasPrefix(trim, "### "):
			flushSummary()
			item = nil
			cur.Groups = append(cur.Groups, Group{Title: strings.TrimSpace(trim[4:])})
		case trim == "":
			flushSummary()
			item = nil
		case bulletRe.MatchString(line):
			flushSummary()
			m := bulletRe.FindStringSubmatch(line)
			g := group()
			g.Items = append(g.Items, Item{Text: strings.TrimSpace(m[2])})
			item = &g.Items[len(g.Items)-1]
		case item != nil && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")):
			item.Text += " " + trim
		default:
			item = nil
			summary = append(summary, trim)
		}
	}
	flushSummary()
	if len(out) == 0 {
		return nil, fmt.Errorf("no release found")
	}
	for i := range out {
		out[i].HTML = Inline(out[i].Summary)
		gs := out[i].Groups[:0]
		for _, g := range out[i].Groups {
			if len(g.Items) == 0 {
				continue
			}
			for j := range g.Items {
				g.Items[j].HTML = Inline(g.Items[j].Text)
			}
			gs = append(gs, g)
		}
		out[i].Groups = gs
	}
	return out, nil
}

// MustParse is Parse for an embedded file; it panics on error (the app's
// changelog test catches that before release).
func MustParse(md string) []Release {
	rs, err := Parse(md)
	if err != nil {
		panic("relnotes: " + err.Error())
	}
	return rs
}

// Base strips a leading "v" and the "-BUILD" suffix: "v1.2.0-45" → "1.2.0".
func Base(version string) string {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if i := strings.Index(v, "-"); i > 0 {
		v = v[:i]
	}
	return v
}

// Check fails unless the changelog parses and has an entry for version (the
// VERSION file, with or without the build suffix). Call it from a test.
func Check(md, version string) error {
	rs, err := Parse(md)
	if err != nil {
		return err
	}
	want := Base(version)
	for _, r := range rs {
		if r.Version == want {
			return nil
		}
	}
	return fmt.Errorf("CHANGELOG.md has no \"## %s\" entry: add one when you bump VERSION", want)
}

var (
	codeRe = regexp.MustCompile("`([^`]+)`")
	boldRe = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	linkRe = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
)

// Inline renders the markdown we allow in a note: `code`, **bold** and
// [text](url) with http(s), relative or anchor URLs. Everything else is text.
func Inline(s string) template.HTML {
	// Pull code spans out first so nothing inside them is formatted.
	var codes []string
	s = codeRe.ReplaceAllStringFunc(s, func(m string) string {
		codes = append(codes, m[1:len(m)-1])
		return fmt.Sprintf("\x00%d\x00", len(codes)-1)
	})
	s = html.EscapeString(s)
	s = linkRe.ReplaceAllStringFunc(s, func(m string) string {
		p := linkRe.FindStringSubmatch(m)
		u := html.UnescapeString(p[2])
		if !(strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "/") || strings.HasPrefix(u, "#")) {
			return m
		}
		return `<a href="` + html.EscapeString(u) + `">` + p[1] + `</a>`
	})
	s = boldRe.ReplaceAllString(s, "<strong>$1</strong>")
	for i, c := range codes {
		s = strings.Replace(s, fmt.Sprintf("\x00%d\x00", i), "<code>"+html.EscapeString(c)+"</code>", 1)
	}
	return template.HTML(s)
}

// fragmentTmpl is the markup every app uses, so one stylesheet fits all:
// section.release > h2 (version + span.when) > p.summary, h3, ul > li.
var fragmentTmpl = template.Must(template.New("rn").Parse(`<div class="relnotes">
{{- range .}}
<section class="release" id="v{{.Version}}">
  <h2>{{.Version}}{{if .Date}} <span class="when">· <time datetime="{{.Date}}">{{.Date}}</time></span>{{end}}</h2>
  {{- if .HTML}}
  <p class="summary">{{.HTML}}</p>{{end}}
  {{- range .Groups}}{{if .Title}}
  <h3>{{.Title}}</h3>{{end}}
  <ul>{{range .Items}}
    <li>{{.HTML}}</li>{{end}}
  </ul>{{end}}
</section>
{{- end}}
</div>`))

// HTML renders the releases with the shared markup.
func HTML(rs []Release) template.HTML {
	var b strings.Builder
	if err := fragmentTmpl.Execute(&b, rs); err != nil {
		return template.HTML("<p>" + html.EscapeString(err.Error()) + "</p>")
	}
	return template.HTML(b.String())
}

// JSONHandler serves {"app","version","releases":[…]} at /release-notes.json.
func JSONHandler(app, version string, rs []Release) http.Handler {
	body, _ := json.MarshalIndent(struct {
		App      string    `json:"app"`
		Version  string    `json:"version"`
		Releases []Release `json:"releases"`
	}{app, version, rs}, "", "  ")
	body = append(body, '\n')
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write(body)
	})
}

// PageOptions describe a standalone page for a single-page tool.
type PageOptions struct {
	App     string // "pwgen"
	Domain  string // "gutocarvalho.net", shown after the name
	Version string // full X.Y.Z-BUILD
	Source  string // repository URL
	LangKey string // localStorage key the tool uses for the language ("pwgen-lang")
	Style   string // the tool's own CSS, so the page looks like the tool (see StyleOf)
}

var styleRe = regexp.MustCompile(`(?s)<style[^>]*>(.*?)</style>`)

// StyleOf returns the CSS of the first <style> block of a page.
func StyleOf(page string) string {
	if m := styleRe.FindStringSubmatch(page); m != nil {
		return m[1]
	}
	return ""
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="pt">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.O.App}} · novidades / release notes</title>
<meta name="robots" content="noindex">
<style>{{.Style}}</style>
<style>
  :root:not([lang="en"]) .i-en{display:none}
  :root[lang="en"] .i-pt{display:none}
  .rn-wrap{max-width:760px;margin:0 auto;padding:36px 22px 60px}
  .rn-wrap h1{font-size:28px;margin:0 0 6px}
  .rn-wrap h1 a{color:inherit;text-decoration:none}
  .rn-wrap h1 .rn-dom{color:var(--faint,#8a94a6);font-weight:400}
  .rn-lede{color:var(--muted,#9aa4b2);margin:0 0 26px}
  .relnotes .release{border-top:1px solid var(--line,#2a3446);padding:18px 0 6px}
  .relnotes h2{font-family:var(--mono,ui-monospace,monospace);font-size:17px;margin:0 0 8px;color:var(--accent-ink,inherit)}
  .relnotes .when{color:var(--faint,#8a94a6);font-weight:400;font-size:13px}
  .relnotes h3{font-size:13px;text-transform:uppercase;letter-spacing:.06em;color:var(--muted,#9aa4b2);margin:12px 0 4px}
  .relnotes ul{margin:4px 0 10px;padding-left:20px;line-height:1.6}
  .relnotes li{margin:3px 0}
  .relnotes code{font-family:var(--mono,ui-monospace,monospace);font-size:.9em}
  .relnotes a{color:var(--accent-ink,inherit)}
  .rn-foot{margin-top:30px;padding-top:16px;border-top:1px solid var(--line,#2a3446);font-family:var(--mono,ui-monospace,monospace);font-size:12.5px;color:var(--faint,#8a94a6)}
  .rn-foot a{color:var(--muted,#9aa4b2)}
</style>
</head>
<body>
<div class="rn-wrap">
  <div class="langsw" role="group" aria-label="Idioma / Language">
    <button type="button" data-set="pt">PT</button>
    <button type="button" data-set="en">EN</button>
  </div>
  <header>
    <h1 class="brand"><a href="/">{{.O.App}}<span class="rn-dom">{{if .O.Domain}}.{{.O.Domain}}{{end}}</span></a></h1>
    <p class="rn-lede"><span class="i-pt">Novidades: o que mudou em cada versão.</span><span class="i-en">Release notes: what changed in each version.</span></p>
  </header>
  {{.Body}}
  <footer class="rn-foot">
    <a href="/">← {{.O.App}}</a> ·
    <a href="/release-notes.json">JSON</a>{{if .O.Source}} ·
    <a href="{{.O.Source}}/-/blob/main/CHANGELOG.md">CHANGELOG.md</a>{{end}} ·
    {{.O.App}} {{.O.Version}}
  </footer>
</div>
<script>
(function(){
  var key={{.O.LangKey}}, root=document.documentElement, b=document.querySelectorAll(".langsw button");
  function set(l){
    root.setAttribute("lang",l);
    try{ localStorage.setItem(key,l); }catch(e){}
    for(var i=0;i<b.length;i++) b[i].setAttribute("aria-current", b[i].getAttribute("data-set")===l?"true":"false");
  }
  var l=null; try{ l=localStorage.getItem(key); }catch(e){}
  var q=/[?&]lang=(pt|en)/.exec(location.search); if(q) l=q[1];
  if(!l) l=(navigator.language||"pt").toLowerCase().indexOf("pt")===0?"pt":"en";
  set(l);
  for(var i=0;i<b.length;i++) b[i].addEventListener("click",function(){ set(this.getAttribute("data-set")); });
})();
</script>
</body>
</html>
`))

// PageHandler serves the standalone release-notes page of a single-page tool.
func PageHandler(o PageOptions, rs []Release) http.Handler {
	if o.LangKey == "" {
		o.LangKey = o.App + "-lang"
	}
	var b strings.Builder
	err := pageTmpl.Execute(&b, struct {
		O     PageOptions
		Style template.CSS
		Body  template.HTML
	}{o, template.CSS(o.Style), HTML(rs)})
	page := b.String()
	if err != nil {
		page = "relnotes: " + err.Error()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})
}

// Register mounts the page at /release-notes and the JSON at
// /release-notes.json on a single-page tool's mux.
func Register(mux *http.ServeMux, o PageOptions, rs []Release) {
	mux.Handle("/release-notes", PageHandler(o, rs))
	mux.Handle("/release-notes.json", JSONHandler(o.App, o.Version, rs))
}
