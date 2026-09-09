package browse

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/ChronicCmposer/gitd/internal/repo"
	"github.com/ChronicCmposer/gitd/internal/serve"
)

// writeErr maps render/busy/path errors to the right status (R12-Q1, R5-Q3).
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, serve.ErrBusy):
		http.Error(w, "busy", http.StatusServiceUnavailable)
	case errors.Is(err, serve.ErrShuttingDown):
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
	case errors.Is(err, errBadPath):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// resolveRepo validates name and canonicalizes its on-disk dir (R5-Q3). A bad
// name/path is a 404. Returns the resolved dir.
func (h *Handler) resolveRepo(w http.ResponseWriter, name string) (string, bool) {
	dir, err := h.repoDir(name)
	if err != nil {
		writeErr(w, err)
		return "", false
	}
	return dir, true
}

// handleRepoIndex lists all bare repos (200). Routing it through the serve
// channel keeps filesystem reads consistent with the rest of the daemon.
func (h *Handler) handleRepoIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, err := h.renderAction(r, func(ctx context.Context) ([]byte, error) {
		names, err := repo.ListBare(h.reposRoot)
		if err != nil {
			return nil, err
		}
		items := make([]repoItem, 0, len(names))
		for _, n := range names {
			items = append(items, repoItem{Name: n})
		}
		return h.renderPage(pageData{Title: "git.cmposer.cc", Body: frag("repolist", repolistData{Repos: items})})
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeHTML(w, body)
}

// handleRepoHome renders the repo landing page: ref selector, commit log,
// README, and root tree — or the empty-repository page (200, R13-Q7).
func (h *Handler) handleRepoHome(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("repo")
	dir, ok := h.resolveRepo(w, name)
	if !ok {
		return
	}
	ref := r.URL.Query().Get("ref")

	body, err := h.renderAction(r, func(ctx context.Context) ([]byte, error) {
		hasRef, err := h.hasRef(ctx, dir)
		if err != nil {
			return nil, err
		}
		if !hasRef {
			return h.renderPage(pageData{Title: name, Repo: name, RenderMode: h.render, Body: frag("empty", emptyData{Repo: name})})
		}
		if ref == "" {
			ref, err = h.defaultRef(ctx, dir)
			if err != nil {
				return nil, err
			}
		}
		refs, err := h.refs(ctx, dir)
		if err != nil {
			return nil, err
		}
		refbar := frag("refbar", refbarData{Repo: name, Ref: ref, Refs: refs, Tab: "home"})

		commits, err := h.commitLog(ctx, dir, ref, 0)
		if err != nil {
			return nil, err
		}
		logBody := frag("commitlist", commitListData{Repo: name, Ref: ref, Commits: parseCommits(commits)})

		rd, err := h.renderReadme(ctx, dir, ref)
		if err != nil {
			return nil, err
		}
		readmeBody := template.HTML("")
		if rd.found {
			readmeBody = frag("readme", readmeData{
				Repo: name, Ref: ref, Name: rd.name,
				Body:        template.HTML(rd.rendered),
				RawMarkdown: string(rd.rendered),
				IsMarkdown:  rd.isMarkdown, Truncated: rd.truncated,
				RenderMode: h.render, SourceURL: "/" + name + "/raw?ref=" + ref + "&path=" + rd.name,
			})
		}

		entries, err := h.treeEntries(ctx, dir, ref, "")
		if err != nil {
			return nil, err
		}
		treeBody := frag("tree", treeData{Repo: name, Ref: ref, Entries: parseTree(entries, "")})

		var b strings.Builder
		b.WriteString(`<div class="repo-home"><h1>` + htmlEscape(name) + `</h1>`)
		b.WriteString(string(refbar))
		b.WriteString(string(logBody))
		b.WriteString(string(readmeBody))
		b.WriteString(string(treeBody))
		b.WriteString(`</div>`)
		return h.renderPage(pageData{Title: name, Repo: name, RenderMode: h.render, Body: template.HTML(b.String())})
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeHTML(w, body)
}

// handleLog renders the paginated commit log (50/page, R2-Q7).
func (h *Handler) handleLog(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("repo")
	dir, ok := h.resolveRepo(w, name)
	if !ok {
		return
	}
	q := r.URL.Query()
	ref := q.Get("ref")
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 0 {
		page = 0
	}

	body, err := h.renderAction(r, func(ctx context.Context) ([]byte, error) {
		var err error
		if ref == "" {
			ref, err = h.defaultRef(ctx, dir)
			if err != nil {
				return nil, err
			}
		}
		refs, err := h.refs(ctx, dir)
		if err != nil {
			return nil, err
		}
		commits, err := h.commitLog(ctx, dir, ref, page*pageSizeCommits)
		if err != nil {
			return nil, err
		}
		items := parseCommits(commits)
		hasNext := len(items) == pageSizeCommits
		refbar := frag("refbar", refbarData{Repo: name, Ref: ref, Refs: refs, Tab: "log"})
		body := frag("commitlist", commitListData{Repo: name, Ref: ref, Commits: items, Page: page, HasNext: hasNext})
		var b strings.Builder
		b.WriteString(string(refbar))
		b.WriteString(string(body))
		return h.renderPage(pageData{Title: name + " · log", Repo: name, RenderMode: h.render, Body: template.HTML(b.String())})
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeHTML(w, body)
}

// handleTree renders a directory listing (500 entries/page, R2-Q7).
func (h *Handler) handleTree(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("repo")
	dir, ok := h.resolveRepo(w, name)
	if !ok {
		return
	}
	q := r.URL.Query()
	ref := q.Get("ref")
	tp, err := cleanRepoPath(q.Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}

	body, err := h.renderAction(r, func(ctx context.Context) ([]byte, error) {
		var err error
		if ref == "" {
			ref, err = h.defaultRef(ctx, dir)
			if err != nil {
				return nil, err
			}
		}
		refs, err := h.refs(ctx, dir)
		if err != nil {
			return nil, err
		}
		entries, err := h.treeEntries(ctx, dir, ref, tp)
		if err != nil {
			return nil, err
		}
		refbar := frag("refbar", refbarData{Repo: name, Ref: ref, Refs: refs, Tab: "tree"})
		tree := frag("tree", treeData{Repo: name, Ref: ref, Path: tp, Entries: parseTree(entries, tp), HasPath: tp != ""})
		var b strings.Builder
		b.WriteString(string(refbar))
		b.WriteString(string(tree))
		return h.renderPage(pageData{Title: name + " · tree", Repo: name, RenderMode: h.render, Body: template.HTML(b.String())})
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeHTML(w, body)
}

// handleBlob renders a file: markdown files honor the render toggle; others
// show as plain text. 256KiB cap (R2-Q7).
func (h *Handler) handleBlob(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("repo")
	dir, ok := h.resolveRepo(w, name)
	if !ok {
		return
	}
	q := r.URL.Query()
	ref := q.Get("ref")
	tp, err := cleanRepoPath(q.Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if tp == "" {
		writeErr(w, errBadPath)
		return
	}

	body, err := h.renderAction(r, func(ctx context.Context) ([]byte, error) {
		if ref == "" {
			ref, err = h.defaultRef(ctx, dir)
			if err != nil {
				return nil, err
			}
		}
		data, err := h.blobAt(ctx, dir, ref, tp)
		if err != nil {
			return nil, err
		}
		truncated := len(data) > maxRenderBytes
		if truncated {
			data = data[:maxRenderBytes]
		}
		isMarkdown := strings.HasSuffix(strings.ToLower(tp), ".md")
		rawURL := "/" + name + "/raw?ref=" + ref + "&path=" + tp
		var bodyHTML template.HTML
		switch {
		case isMarkdown && h.render == "server":
			rendered, err := renderMarkdownServer(data)
			if err != nil {
				return nil, err
			}
			bodyHTML = template.HTML(rendered)
		default:
			bodyHTML = template.HTML(escapeHTML(data))
		}
		b := frag("blob", blobData{
			Repo: name, Ref: ref, Path: tp,
			RawMarkdown: string(data), Body: bodyHTML,
			IsMarkdown: isMarkdown, Truncated: truncated,
			RenderMode: h.render, RawURL: rawURL,
		})
		return h.renderPage(pageData{Title: name + " · " + tp, Repo: name, RenderMode: h.render, Body: b})
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeHTML(w, body)
}

// handleRaw serves the raw blob as a Content-Disposition attachment (R2-Q6:
// everything outside the markdown+code/text whitelist is an attachment).
func (h *Handler) handleRaw(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("repo")
	dir, ok := h.resolveRepo(w, name)
	if !ok {
		return
	}
	q := r.URL.Query()
	ref := q.Get("ref")
	tp, err := cleanRepoPath(q.Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}

	data, err := h.renderAction(r, func(ctx context.Context) ([]byte, error) {
		var err error
		if ref == "" {
			ref, err = h.defaultRef(ctx, dir)
			if err != nil {
				return nil, err
			}
		}
		return h.blobAt(ctx, dir, ref, tp)
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(fileBase(tp)))
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

// handleDiff renders a commit diff, capped at 256KiB with a raw download link
// (R11-Q9). raw=1 serves the full diff as an attachment.
func (h *Handler) handleDiff(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("repo")
	dir, ok := h.resolveRepo(w, name)
	if !ok {
		return
	}
	q := r.URL.Query()
	commit := q.Get("commit")
	if commit == "" {
		writeErr(w, errBadPath)
		return
	}

	truncated := false
	diff, err := h.renderAction(r, func(ctx context.Context) ([]byte, error) {
		out, tr, err := h.diffOf(ctx, dir, commit)
		truncated = tr
		return out, err
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	if q.Get("raw") == "1" {
		// Raw download link serves the FULL diff (R11-Q9), untruncated.
		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(commit+".diff"))
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(diff)
		return
	}
	// Page display is capped at 256KiB (R11-Q9).
	display := diff
	if truncated {
		display = display[:maxRenderBytes]
	}

	body, err := h.renderAction(r, func(ctx context.Context) ([]byte, error) {
		body := frag("diff", diffData{
			Repo: name, Commit: commit, Body: highlightDiff(display),
			Truncated: truncated, RawURL: "/" + name + "/diff?commit=" + commit + "&raw=1",
		})
		return h.renderPage(pageData{Title: name + " · diff", Repo: name, RenderMode: h.render, Body: body})
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeHTML(w, body)
}

func writeHTML(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body)
}
