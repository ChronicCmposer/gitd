package browse

import (
	"bytes"
	"html"
	"html/template"
	"strings"
)

// Data types shared by the fragment templates. Field names are the template
// data; escaping is automatic via html/template.

type commitItem struct {
	Hash   string
	Short  string
	Author string
	Date   string
	Msg    string
}

type refbarData struct {
	Repo string
	Ref  string
	Refs []string
	Tab  string // log | tree | home
}

type treeEntry struct {
	Type string // blob | tree
	Name string
	Path string
}

type treeData struct {
	Repo    string
	Ref     string
	Path    string
	Entries []treeEntry
	HasPath bool
}

type readmeData struct {
	Repo        string
	Ref         string
	Name        string
	Body        template.HTML // server-rendered HTML
	RawMarkdown string        // client mode raw markdown (attr-escaped by template)
	IsMarkdown  bool
	Truncated   bool
	RenderMode  string
	SourceURL   string
}

type commitListData struct {
	Repo    string
	Ref     string
	Commits []commitItem
	Page    int
	HasNext bool
}

type repoItem struct {
	Name string
}

type repolistData struct {
	Repos []repoItem
}

type blobData struct {
	Repo        string
	Ref         string
	Path        string
	RawMarkdown string
	Body        template.HTML
	IsMarkdown  bool
	Truncated   bool
	RenderMode  string
	RawURL      string
}

type diffData struct {
	Repo      string
	Commit    string
	Body      template.HTML
	Truncated bool
	RawURL    string
}

type emptyData struct {
	Repo string
}

// fragTmpl holds all the named body fragments. It auto-escapes all fields.
var fragTmpl = template.Must(template.New("fragments").Funcs(template.FuncMap{
	"add":    func(a, b int) int { return a + b },
	"sub":    func(a, b int) int { return a - b },
	"parent": parentPath,
}).Parse(`
{{define "repolist"}}
<div class="repo-index">
<h1>repositories</h1>
{{if .Repos}}
<ul class="repo-list">
{{range .Repos}}<li><a class="name" href="/{{.Name}}">{{.Name}}</a><span class="meta">browse</span></li>{{end}}
</ul>
{{else}}
<div class="empty-repo"><p>no repositories yet.</p><p>create one with <code>git init --bare</code> or push to a new name.</p></div>
{{end}}
</div>
<div class="footer">gitd — personal git server · mTLS browse</div>
{{end}}

{{define "refbar"}}
<div class="refbar">
  <div class="tabs">
    <a href="/{{.Repo}}" class="{{if eq .Tab "home"}}active{{end}}">home</a>
    <a href="/{{.Repo}}/log?ref={{.Ref}}" class="{{if eq .Tab "log"}}active{{end}}">log</a>
    <a href="/{{.Repo}}/tree?ref={{.Ref}}" class="{{if eq .Tab "tree"}}active{{end}}">tree</a>
  </div>
  {{if .Refs}}
  <label class="refsel">ref
    <select onchange="if(this.value){location=this.value;}">
      <option value="">{{.Ref}}</option>
      {{range .Refs}}<option value="/{{$.Repo}}/tree?ref={{.}}">{{.}}</option>{{end}}
    </select>
  </label>
  {{end}}
</div>
{{end}}

{{define "commitlist"}}
<div class="panel">
  <div class="panel-title">commits · {{.Ref}}</div>
  {{if .Commits}}
  <ul class="commit-list">
    {{range .Commits}}
    <li>
      <a class="commit-hash" href="/{{$.Repo}}/diff?ref={{$.Ref}}&commit={{.Hash}}" title="{{.Hash}}">{{.Short}}</a>
      <span class="commit-msg">{{.Msg}}</span>
      <span class="commit-meta">{{.Author}} · {{.Date}}</span>
    </li>
    {{end}}
  </ul>
  <div class="pager">
    {{if .Page}}<a href="/{{.Repo}}/log?ref={{.Ref}}&page={{sub .Page 1}}">&larr; newer</a>{{end}}
    {{if .HasNext}}<a href="/{{.Repo}}/log?ref={{.Ref}}&page={{add .Page 1}}">older &rarr;</a>{{end}}
  </div>
  {{else}}
  <div class="panel-body"><p class="notice">no commits on this ref.</p></div>
  {{end}}
</div>
{{end}}

{{define "tree"}}
<div class="panel">
  <div class="panel-title">tree · {{.Ref}}{{if .Path}} / {{.Path}}{{end}}</div>
  <ul class="tree-list">
  {{if .HasPath}}<li><span class="tree-type dir">d</span><a class="tree-link" href="/{{.Repo}}/tree?ref={{.Ref}}&path={{parent .Path}}">../</a></li>{{end}}
  {{range .Entries}}
  <li>
    {{if eq .Type "tree"}}<span class="tree-type dir">d</span><a class="tree-link" href="/{{$.Repo}}/tree?ref={{$.Ref}}&path={{.Path}}">{{.Name}}/</a>
    {{else}}<span class="tree-type">f</span><a class="tree-link" href="/{{$.Repo}}/blob?ref={{$.Ref}}&path={{.Path}}">{{.Name}}</a>{{end}}
  </li>
  {{end}}
  </ul>
  {{if not .Entries}}{{if not .HasPath}}<div class="panel-body"><p class="notice">empty directory.</p></div>{{end}}{{end}}
</div>
{{end}}

{{define "readme"}}
<div class="panel readme">
  <div class="panel-title">{{.Name}}{{if .Truncated}} · truncated to 256KiB — <a href="{{.SourceURL}}">raw</a>{{end}}</div>
  {{if eq .RenderMode "client"}}
  <div class="markdown" data-markdown="{{.RawMarkdown}}"></div>
  {{else}}
  <div class="markdown">{{.Body}}</div>
  {{end}}
</div>
{{end}}

{{define "blob"}}
<div class="panel">
  <div class="panel-title">{{.Path}}{{if .Truncated}} · truncated to 256KiB — <a href="{{.RawURL}}">raw</a>{{end}}</div>
  {{if .IsMarkdown}}
    {{if eq .RenderMode "client"}}
    <div class="markdown" data-markdown="{{.RawMarkdown}}"></div>
    {{else}}
    <div class="markdown">{{.Body}}</div>
    {{end}}
  {{else}}
  <pre class="blob">{{.Body}}</pre>
  {{end}}
</div>
{{end}}

{{define "diff"}}
<div class="panel">
  <div class="panel-title">diff · {{.Commit}}{{if .Truncated}} · truncated to 256KiB — <a href="{{.RawURL}}">raw diff</a>{{end}}</div>
  <div class="diff"><pre>{{.Body}}</pre></div>
</div>
{{end}}

{{define "empty"}}
<div class="empty-repo">
<h1>{{.Repo}}</h1>
<p>This repository is empty — no refs yet.</p>
<div class="hint"><code>git remote add origin git@git.cmposer.cc:{{.Repo}}.git<br>git push -u origin main</code></div>
<p class="mirror-note">Mirror state: no refs, bundle skipped.</p>
</div>
{{end}}
`))

// frag executes one named fragment and returns it as safe HTML. On error it
// returns an escaped error string so a template bug can never render raw data.
func frag(name string, data any) template.HTML {
	var buf bytes.Buffer
	if err := fragTmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return template.HTML(html.EscapeString("template error: " + err.Error()))
	}
	return template.HTML(buf.String())
}

// parentPath returns the parent directory of a repo path ("" for root).
func parentPath(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ""
	}
	return p[:i]
}
