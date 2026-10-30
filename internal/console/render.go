package console

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/diffsec/sibyl/internal/router"
)

// inProcess sends a client request to the System API handler without a
// network connection.
type inProcess struct{ h http.Handler }

// recorder is the response of an in-process call.
type recorder struct {
	code   int
	header http.Header
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (t inProcess) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := &recorder{header: http.Header{}}
	in := req.Clone(req.Context())
	in.RequestURI = req.URL.RequestURI()
	in.RemoteAddr = "127.0.0.1:0"
	t.h.ServeHTTP(rec, in)
	if rec.code == 0 {
		rec.code = http.StatusOK
	}
	return &http.Response{
		StatusCode: rec.code, Status: http.StatusText(rec.code), Header: rec.header,
		Body: io.NopCloser(&rec.body), ContentLength: int64(rec.body.Len()),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Request: req,
	}, nil
}

var funcs = template.FuncMap{
	"join": strings.Join,
	"usd":  func(f float64) string { return fmt.Sprintf("$%.2f", f) },
	"usd4": func(f float64) string { return fmt.Sprintf("$%.4f", f) },
	"pct":  func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
	"secs": func(ms int64) string { return fmt.Sprintf("%.1f s", float64(ms)/1000) },
	"clock": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Local().Format("15:04:05")
	},
	"sortedKeys": func(m any) []string {
		var keys []string
		switch m := m.(type) {
		case map[string]int:
			for k := range m {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		return keys
	},
	"add":      func(a, b int) int { return a + b },
	"whenText": func(w router.When) string { return w.Text() },
	"has":      func(list []string, v string) bool { return slices.Contains(list, v) },
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			k, _ := kv[i].(string)
			m[k] = kv[i+1]
		}
		return m
	},
}

// parsePages parses each page with the layout. A page file defines
// "title" and "content", and may define partials that htmx asks for.
func parsePages() map[string]*template.Template {
	layout := template.Must(template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html"))
	pages := map[string]*template.Template{}
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		panic(err)
	}
	for _, n := range names {
		name := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
		if name == "layout" {
			continue
		}
		pages[name] = template.Must(template.Must(layout.Clone()).ParseFS(templateFS, n))
	}
	return pages
}

// render writes a page in the layout, or one of its partials for htmx.
func (c *Console) render(w http.ResponseWriter, r *http.Request, code int, page string, data map[string]any) {
	c.renderBlock(w, r, code, page, "layout.html", data)
}

func (c *Console) renderBlock(w http.ResponseWriter, r *http.Request, code int, page, block string, data map[string]any) {
	t, ok := c.pages[page]
	if !ok {
		http.Error(w, "no page "+page, http.StatusInternalServerError)
		return
	}
	data["Page"] = page
	data["User"] = userOf(r)
	if _, set := data["Flash"]; !set {
		data["Flash"] = r.URL.Query().Get("done")
	}
	var b bytes.Buffer
	if err := t.ExecuteTemplate(&b, block, data); err != nil {
		c.Log.Error("console page", "page", page, "err", err)
		http.Error(w, "the page could not be made", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(b.Bytes())
}
