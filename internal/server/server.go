// Package server implements the HTTP UI.
package server

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/LaurensBosscher/read_the_room/internal/article"
	"github.com/LaurensBosscher/read_the_room/internal/bypass"
	"github.com/LaurensBosscher/read_the_room/internal/rules"
	"github.com/LaurensBosscher/read_the_room/internal/scriptlets"
	"github.com/LaurensBosscher/read_the_room/internal/update"
)

//go:embed templates/*.html
var templateFS embed.FS

// Server holds the HTTP dependencies.
type Server struct {
	Rules    *rules.Store
	Updater  *update.Updater
	Bypasser *bypass.Bypasser

	tmpl  *template.Template
	icons map[int][]byte
}

// New builds a Server.
func New(r *rules.Store, u *update.Updater, b *bypass.Bypasser) (*Server, error) {
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s := &Server{Rules: r, Updater: u, Bypasser: b, tmpl: tmpl, icons: map[int][]byte{}}
	for _, size := range []int{192, 512} {
		png, err := makeIcon(size)
		if err != nil {
			return nil, err
		}
		s.icons[size] = png
	}
	return s, nil
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("POST /update", s.updateRules)
	mux.HandleFunc("GET /read", s.read)
	mux.HandleFunc("GET /share", s.share)
	mux.HandleFunc("GET /manifest.webmanifest", s.manifest)
	mux.HandleFunc("GET /sw.js", s.serviceWorker)
	mux.HandleFunc("GET /scriptlets.js", s.scriptletJS)
	mux.HandleFunc("GET /icon-192.png", s.icon(192))
	mux.HandleFunc("GET /icon-512.png", s.icon(512))
	mux.HandleFunc("GET /favicon.ico", s.icon(192))
	mux.HandleFunc("GET /", s.prefix)
	return mux
}

type sourceRow struct {
	Name      string
	Type      string
	Sites     int
	Updated   string
	LastError string
}

type indexData struct {
	Origin      string
	Error       string
	Notice      string
	Bookmarklet template.URL
	Sources     []sourceRow
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	notice := ""
	if r.URL.Query().Get("updated") == "1" {
		notice = "Rules updated"
	}
	s.writeIndex(w, r, http.StatusOK, "", notice)
}

func (s *Server) writeIndex(w http.ResponseWriter, r *http.Request, code int, errMsg, notice string) {
	origin := origin(r)
	data := indexData{
		Origin:  origin,
		Error:   errMsg,
		Notice:  notice,
		Sources: s.sources(),
	}
	bm := "javascript:location.href='" + template.JSEscapeString(origin) +
		"/read?url='+encodeURIComponent(location.href)"
	data.Bookmarklet = template.URL(bm)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	s.execute(w, "index.html", data)
}

func (s *Server) sources() []sourceRow {
	if s.Updater == nil {
		return nil
	}
	var rows []sourceRow
	for _, st := range s.Updater.Status() {
		updated := "never"
		if !st.UpdatedAt.IsZero() {
			updated = st.UpdatedAt.UTC().Format("2006-01-02 15:04 UTC")
		}
		rows = append(rows, sourceRow{
			Name:      st.Name,
			Type:      st.Type,
			Sites:     st.Sites,
			Updated:   updated,
			LastError: st.LastError,
		})
	}
	return rows
}

func (s *Server) updateRules(w http.ResponseWriter, r *http.Request) {
	if s.Updater == nil {
		s.writeIndex(w, r, http.StatusInternalServerError, "Updater unavailable", "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 80*time.Second)
	defer cancel()
	_, err := s.Updater.RunManual(ctx)
	var rate *update.RateLimitError
	switch {
	case errors.Is(err, update.ErrBusy):
		s.writeIndex(w, r, http.StatusConflict, "Update already running", "")
	case errors.As(err, &rate):
		mins := int((rate.Wait + time.Minute - 1) / time.Minute)
		s.writeIndex(w, r, http.StatusTooManyRequests,
			fmt.Sprintf("Updated recently; try again in %d min", mins), "")
	case err != nil:
		s.writeIndex(w, r, http.StatusBadGateway, "Update failed: "+err.Error(), "")
	default:
		http.Redirect(w, r, "/?updated=1", http.StatusSeeOther)
	}
}

func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	target, err := parseTarget(q.Get("url"))
	if err != nil {
		s.writeIndex(w, r, http.StatusBadRequest, "Invalid URL", "")
		return
	}
	view := q.Get("view")
	if view != "original" {
		view = "reader"
	}
	strategy := q.Get("strategy")
	set := s.Rules.Current()
	if strategy != "" && !knownStrategy(set, strategy) {
		s.writeIndex(w, r, http.StatusBadRequest, "Unknown strategy", "")
		return
	}

	res, err := s.Bypasser.Run(r.Context(), target, strategy)
	if err != nil {
		var failed *bypass.FailedError
		if errors.As(err, &failed) {
			s.writeFailure(w, r, target, failed.Attempts)
			return
		}
		s.writeFailure(w, r, target, []bypass.Attempt{{Strategy: "all", Status: "error: " + err.Error()}})
		return
	}

	origin := origin(r)
	links := article.Links{Self: origin, Target: target, View: view}
	banner := s.banner(origin, target, res, view)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")

	if view == "original" {
		out, err := article.Original(res.Resp.HTML, res.Resp.BaseURL, res.Rule, links, banner)
		if err != nil {
			s.writeFailure(w, r, target, res.Attempts)
			return
		}
		if !res.Rule.KeepScripts {
			w.Header().Set("Content-Security-Policy", "script-src 'none'; object-src 'none'")
		}
		_, _ = w.Write([]byte(out))
		return
	}

	art, err := article.Extract(res.Resp.HTML, res.Resp.BaseURL, res.Rule, &links)
	if err != nil {
		s.writeFailure(w, r, target, res.Attempts)
		return
	}
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src * data: blob:; media-src *; style-src 'unsafe-inline'; frame-src https:; base-uri 'none'; form-action 'self'")
	s.execute(w, "reader.html", struct {
		Title, Byline, SiteName string
		Content                 template.HTML
		Banner                  template.HTML
	}{
		Title:    art.Title,
		Byline:   art.Byline,
		SiteName: art.SiteName,
		Content:  art.ContentHTML,
		Banner:   template.HTML(banner),
	})
}

func (s *Server) banner(origin string, target *url.URL, res *bypass.Result, view string) string {
	other, toggleName := "original", "Original view"
	if view == "original" {
		other, toggleName = "reader", "Reader view"
	}
	tries := make([]tryLink, 0, len(s.Rules.Current().KnownStrategies()))
	for _, st := range s.Rules.Current().KnownStrategies() {
		tries = append(tries, tryLink{
			Name: st,
			URL:  readURL(origin, target, view, st),
		})
	}
	var buf bytes.Buffer
	_ = s.tmpl.ExecuteTemplate(&buf, "banner.html", struct {
		Home       string
		Strategy   string
		Rule       string
		Complete   bool
		Toggle     string
		ToggleName string
		Source     string
		Tries      []tryLink
		Attempts   []bypass.Attempt
	}{
		Home:       origin + "/",
		Strategy:   res.Strategy,
		Rule:       res.Rule.Name,
		Complete:   res.Complete,
		Toggle:     readURL(origin, target, other, res.Strategy),
		ToggleName: toggleName,
		Source:     target.String(),
		Tries:      tries,
		Attempts:   res.Attempts,
	})
	return buf.String()
}

type tryLink struct {
	Name string
	URL  string
}

func (s *Server) writeFailure(w http.ResponseWriter, r *http.Request, target *url.URL, attempts []bypass.Attempt) {
	origin := origin(r)
	tries := make([]tryLink, 0, len(s.Rules.Current().KnownStrategies()))
	for _, st := range s.Rules.Current().KnownStrategies() {
		tries = append(tries, tryLink{Name: st, URL: readURL(origin, target, "reader", st)})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadGateway)
	s.execute(w, "error.html", struct {
		Error    string
		Target   string
		Attempts []bypass.Attempt
		Tries    []tryLink
	}{
		Error:    "No strategy produced a usable page.",
		Target:   target.String(),
		Attempts: attempts,
		Tries:    tries,
	})
}

func (s *Server) share(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	found := q.Get("url")
	if found == "" {
		if m := linkRE.FindString(q.Get("text")); m != "" {
			found = m
		} else if m := linkRE.FindString(q.Get("title")); m != "" {
			found = m
		}
	}
	if found == "" {
		s.writeIndex(w, r, http.StatusBadRequest, "No link found in shared content", "")
		return
	}
	http.Redirect(w, r, "/read?url="+url.QueryEscape(found), http.StatusSeeOther)
}

func (s *Server) prefix(w http.ResponseWriter, r *http.Request) {
	target, ok := targetFromPath(r.URL.EscapedPath(), r.URL.RawQuery)
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/read?url="+url.QueryEscape(target), http.StatusSeeOther)
}

const manifestJSON = `{"name":"Read the Room","short_name":"ReadRoom","start_url":"/","scope":"/","display":"standalone","background_color":"#111827","theme_color":"#6d28d9","icons":[{"src":"/icon-192.png","sizes":"192x192","type":"image/png"},{"src":"/icon-512.png","sizes":"512x512","type":"image/png","purpose":"any maskable"}],"share_target":{"action":"/share","method":"GET","params":{"title":"title","text":"text","url":"url"}}}`

func (s *Server) manifest(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	_, _ = w.Write([]byte(manifestJSON))
}

const serviceWorkerJS = `self.addEventListener('install',()=>self.skipWaiting());self.addEventListener('fetch',()=>{});`

func (s *Server) serviceWorker(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write([]byte(serviceWorkerJS))
}

func (s *Server) scriptletJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(scriptlets.JS())
}

func (s *Server) icon(size int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		png := s.icons[size]
		if png == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}
}

func (s *Server) execute(w http.ResponseWriter, name string, data any) {
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

var (
	linkRE   = regexp.MustCompile(`https?://\S+`)
	schemeRE = regexp.MustCompile(`^(https?):/+`)
)

func origin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func parseTarget(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty url")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid url")
	}
	return u, nil
}

func knownStrategy(set *rules.Set, name string) bool {
	for _, s := range set.KnownStrategies() {
		if s == name {
			return true
		}
	}
	return false
}

func readURL(origin string, target *url.URL, view, strategy string) string {
	u := origin + "/read?url=" + url.QueryEscape(target.String()) + "&view=" + url.QueryEscape(view)
	if strategy != "" {
		u += "&strategy=" + url.QueryEscape(strategy)
	}
	return u
}

// targetFromPath undoes ServeMux's "//" cleaning for prefix URLs.
func targetFromPath(escapedPath, rawQuery string) (string, bool) {
	p := strings.TrimPrefix(escapedPath, "/")
	m := schemeRE.FindStringSubmatch(p)
	if m == nil {
		return "", false
	}
	p = m[1] + "://" + p[len(m[0]):]
	u, err := url.PathUnescape(p)
	if err != nil {
		return "", false
	}
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u, true
}

func makeIcon(size int) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{R: 0x6d, G: 0x28, B: 0xd9, A: 0xff}
	white := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	for y := range size {
		for x := range size {
			img.Set(x, y, bg)
		}
	}
	w, h := float64(size), float64(size)
	railW := 0.08 * w
	left, right := 0.30*w, 0.70*w
	top, bottom := 0.15*h, 0.85*h
	fillRect(img, int(left-railW/2), int(top), int(railW), int(bottom-top), white)
	fillRect(img, int(right-railW/2), int(top), int(railW), int(bottom-top), white)
	rungH := 0.06 * h
	for i := 1; i <= 4; i++ {
		cy := top + 0.70*h*float64(i)/5.0
		fillRect(img, int(left-railW/2), int(cy-rungH/2), int(right-left+railW), int(rungH), white)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func fillRect(img *image.RGBA, x, y, w, h int, c color.Color) {
	b := img.Bounds()
	for yy := y; yy < y+h; yy++ {
		if yy < b.Min.Y || yy >= b.Max.Y {
			continue
		}
		for xx := x; xx < x+w; xx++ {
			if xx < b.Min.X || xx >= b.Max.X {
				continue
			}
			img.Set(xx, yy, c)
		}
	}
}
