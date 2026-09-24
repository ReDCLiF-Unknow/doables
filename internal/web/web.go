package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"doables/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// manifestJSON makes Doables installable: "Add to Home Screen" (or "Install"
// in Chrome/Edge) gives it an icon and a window without browser chrome. There
// is no service worker, so it is not usable offline.
const manifestJSON = `{
  "name": "Doables",
  "short_name": "Doables",
  "description": "Shareable to-do lists you can finish together",
  "start_url": "/",
  "scope": "/",
  "display": "standalone",
  "background_color": "#151f2c",
  "theme_color": "#1d273b",
  "icons": [
    {"src": "/static/icon-192.png", "sizes": "192x192", "type": "image/png", "purpose": "any maskable"},
    {"src": "/static/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "any maskable"}
  ]
}`

func serveManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	io.WriteString(w, manifestJSON)
}

const cookieName = "doables_token"

var avatarColors = []string{"blue", "azure", "indigo", "purple", "pink", "red", "orange", "yellow", "lime", "green", "teal", "cyan"}

var funcs = template.FuncMap{
	// color picks a stable Tabler colour name for a user.
	"color": func(id int64) string { return avatarColors[int(id%int64(len(avatarColors)))] },
	// initial is the first letter of a name, upper-cased, for avatars.
	"initial": func(name string) string {
		r, _ := utf8.DecodeRuneInString(name)
		if r == utf8.RuneError {
			return "?"
		}
		return strings.ToUpper(string(r))
	},
	// dueLabel renders a YYYY-MM-DD due date relative to today.
	"dueLabel": dueLabel,
	// dueClass picks the badge colours for a due date.
	"dueClass": dueClass,
	// when says how long ago something happened, as a conversation would.
	"when": func(t time.Time) string { return when(t, time.Now()) },
	// plural puts a count in front of a word, adding an s unless it is one.
	"plural": func(n int, word string) string {
		if n == 1 {
			return "1 " + word
		}
		return strconv.Itoa(n) + " " + word + "s"
	},
	// row bundles a task with the page it is shown on, for the "taskrow" template.
	"row": func(p pageData, t store.Task) taskRow {
		return taskRow{Task: t, Today: p.Today, Path: p.Path, ShowList: p.View != "list",
			ShowWho: p.Current != nil && p.Current.Members > 1,
			Me:      p.User.ID, Members: p.Members,
			ShowThread: p.View == "list", Thread: p.Threads[t.ID],
			Tag: p.Tag, Filter: p.Filter}
	},
	// tagged shows a task's title or description with its #tags as links.
	"tagged": tagged,
	// listURL and tagURL are the addresses of a list shown a particular way.
	"listURL": listURL,
	"tagURL":  tagURL,
}

// listURL is the address of a list showing only the tasks with tag (none when
// it is empty) that are open, done or either ("all").
func listURL(listID int64, tag, filter string) string {
	q := url.Values{}
	if tag != "" {
		q.Set("tag", tag)
	}
	if filter == "open" || filter == "done" {
		q.Set("filter", filter)
	}
	if len(q) == 0 {
		return listPath(listID)
	}
	return listPath(listID) + "?" + q.Encode()
}

// tagURL is where clicking tag takes you: the list showing only that tag,
// still open or done as it was. Clicking the tag already being shown goes back
// to every task, so a second click undoes the first.
func tagURL(listID int64, tag, current, filter string) string {
	if tag == current {
		tag = ""
	}
	return listURL(listID, tag, filter)
}

// tagged renders text with each #tag in it as a link that shows the task's
// list filtered to that tag. Everything else is escaped as usual.
func tagged(text string, row taskRow) template.HTML {
	var b strings.Builder
	last := 0
	for _, m := range store.FindTags(text) {
		b.WriteString(template.HTMLEscapeString(text[last:m.Start]))
		class, title := "hashtag", "Show only tasks tagged "+text[m.Start:m.End]
		if m.Tag == row.Tag {
			class, title = "hashtag active", "Show every task again"
		}
		fmt.Fprintf(&b, `<a class="%s" href="%s" title="%s">%s</a>`, class,
			template.HTMLEscapeString(tagURL(row.Task.ListID, m.Tag, row.Tag, row.Filter)),
			template.HTMLEscapeString(title), template.HTMLEscapeString(text[m.Start:m.End]))
		last = m.End
	}
	b.WriteString(template.HTMLEscapeString(text[last:]))
	return template.HTML(b.String())
}

const dateLayout = "2006-01-02"

func parseDate(s string) (time.Time, bool) {
	t, err := time.Parse(dateLayout, s)
	return t, err == nil
}

// when renders a moment relative to now: "just now", "5 min ago", "14:05",
// "yesterday 14:05", "Sep 22". Times are shown in the server's own zone, as
// "today" is everywhere else.
func when(t, now time.Time) string {
	t, now = t.In(time.Local), now.In(time.Local)
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min ago"
	}
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	if y1 == y2 && m1 == m2 && d1 == d2 {
		return t.Format("15:04")
	}
	if yy, ym, yd := now.AddDate(0, 0, -1).Date(); y1 == yy && m1 == ym && d1 == yd {
		return "yesterday " + t.Format("15:04")
	}
	if y1 == y2 {
		return t.Format("Jan 2")
	}
	return t.Format("Jan 2, 2006")
}

// dueLabel renders a due date relative to today. A finished task is never
// late, so its past dates are shown plainly rather than as "Overdue".
func dueLabel(due, today string, done bool) string {
	d, ok := parseDate(due)
	now, ok2 := parseDate(today)
	if !ok || !ok2 {
		return due
	}
	switch days := int(d.Sub(now).Hours() / 24); {
	case days == 0:
		return "Today"
	case days == 1:
		return "Tomorrow"
	case days == -1 && !done:
		return "Yesterday"
	case days < 0 && !done:
		return "Overdue · " + d.Format("Jan 2")
	case d.Year() != now.Year():
		return d.Format("Jan 2, 2006")
	default:
		return d.Format("Jan 2")
	}
}

func dueClass(due, today string, done bool) string {
	switch {
	case done:
		return "bg-secondary-lt text-secondary"
	case due < today:
		return "bg-red-lt text-red"
	case due == today:
		return "bg-orange-lt text-orange"
	default:
		return "bg-secondary-lt"
	}
}

// taskRow is what the "taskrow" template renders.
type taskRow struct {
	Task     store.Task
	Today    string
	Path     string // page to return to after an action
	ShowList bool   // show which list the task belongs to (Today and Mine)
	ShowWho  bool   // show who added / finished it (shared lists)
	Me       int64  // the viewer, so their own name can read "you"
	// Members of the list being viewed. The assign menu needs them, so it
	// only appears on a list's own page; elsewhere a task's list is not
	// necessarily the one whose members were loaded.
	Members []store.Member
	// The comments, likewise only on a list's own page. Elsewhere the row
	// just says how many there are.
	ShowThread bool
	Thread     []store.Comment
	// The tag and Open/Done filter the list is shown with, so a task's tags
	// can link to the list filtered the same way, or undo the filter.
	Tag, Filter string
}

type Server struct {
	store *store.Store
	tmpl  *template.Template
	mux   *http.ServeMux
	hub   *hub
	// signups limits how fast one address can create identities, the only
	// thing a stranger can do here without having one already.
	signups *limiter
	// sameOrigin refuses requests that change something when a browser sent
	// them from another site. SameSite=Lax stops such a request carrying the
	// victim's cookie, but not its response setting a new one: without this a
	// page elsewhere could sign you in as someone else, replacing your token.
	// Requests with no browser headers at all, like the CLI's, are allowed.
	sameOrigin *http.CrossOriginProtection
	// demoSeed is set when this server is a public demo: it fills a new
	// visitor's sandbox and returns the list to show them first.
	demoSeed func(store.User) (int64, error)
}

// SetDemo turns the server into a public demo. Everyone who picks a name is
// given seed's example lists to play with, and the pages say that nothing
// here lasts. Removing old sandboxes is the caller's job (see package demo).
func (s *Server) SetDemo(seed func(store.User) (int64, error)) { s.demoSeed = seed }

func New(s *store.Store) *Server {
	srv := &Server{
		store:      s,
		tmpl:       template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")),
		mux:        http.NewServeMux(),
		hub:        newHub(),
		signups:    newLimiter(signupBurst, signupRefill),
		sameOrigin: http.NewCrossOriginProtection(),
	}
	s.SetNotifier(srv.hub.publish)
	srv.routes()
	return srv
}

func today() string { return time.Now().Format(dateLayout) }

type userKey struct{}

// ServeHTTP identifies the caller (cookie or bearer token) before routing.
// csp keeps the page to its own origin. The inline scripts and styles need
// 'unsafe-inline', so this does not stop injected script from running; what it
// does stop is a page fetching or sending anything anywhere else, which is
// what an injection would want to do. It is worth having only because there is
// no longer a CDN to allow.
const csp = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; form-action 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// CloseStreams ends every live-update stream, for a server that is shutting
// down: register it with http.Server.RegisterOnShutdown. Ordinary requests are
// left to finish.
func (s *Server) CloseStreams() { s.hub.close() }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := s.sameOrigin.Check(r); err != nil {
		http.Error(w, "refused: that request came from another site", http.StatusForbidden)
		return
	}
	if token := tokenFrom(r); token != "" {
		if u, err := s.store.UserByToken(token); err == nil {
			r = r.WithContext(context.WithValue(r.Context(), userKey{}, &u))
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	// App icons and the install manifest (public: browsers fetch them without cookies)
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/static/", http.FileServerFS(static))
	s.mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
	s.mux.HandleFunc("GET /manifest.webmanifest", serveManifest)

	// Sign-in and sharing
	s.mux.HandleFunc("GET /welcome", s.welcomeGet)
	s.mux.HandleFunc("POST /welcome", s.limited(s.welcomePost, s.tooManySignups))
	s.mux.HandleFunc("POST /welcome/token", s.welcomeToken)
	s.mux.HandleFunc("GET /join/{code}", s.joinGet)
	s.mux.HandleFunc("POST /join/{code}", s.joinPost)
	s.mux.HandleFunc("GET /me", s.authed(s.meGet))
	s.mux.HandleFunc("GET /me/token", s.authed(s.meToken))
	s.mux.HandleFunc("POST /me/token/saved", s.authed(s.meTokenSaved))
	s.mux.HandleFunc("POST /me", s.authed(s.mePost))

	// HTML UI
	s.mux.HandleFunc("GET /{$}", s.authed(s.pageIndex))
	s.mux.HandleFunc("GET /today", s.authed(s.pageToday))
	s.mux.HandleFunc("GET /mine", s.authed(s.pageMine))
	s.mux.HandleFunc("GET /events", s.authed(s.events))
	s.mux.HandleFunc("GET /lists/{id}", s.authed(s.pageList))
	s.mux.HandleFunc("POST /lists", s.authed(s.formCreateList))
	s.mux.HandleFunc("POST /lists/{id}/rename", s.authed(s.formRenameList))
	s.mux.HandleFunc("POST /lists/{id}/delete", s.authed(s.formDeleteList))
	s.mux.HandleFunc("POST /lists/{id}/restore", s.authed(s.formRestoreList))
	s.mux.HandleFunc("POST /lists/{id}/claim", s.authed(s.formClaimList))
	s.mux.HandleFunc("POST /lists/{id}/invite/reset", s.authed(s.formResetInvite))
	s.mux.HandleFunc("POST /lists/{id}/leave", s.authed(s.formLeaveList))
	s.mux.HandleFunc("POST /lists/{id}/members/{uid}/remove", s.authed(s.formRemoveMember))
	s.mux.HandleFunc("POST /lists/{id}/tasks", s.authed(s.formAddTask))
	s.mux.HandleFunc("POST /tasks/{id}/toggle", s.authed(s.formToggleTask))
	s.mux.HandleFunc("POST /tasks/{id}/edit", s.authed(s.formEditTask))
	s.mux.HandleFunc("POST /tasks/{id}/assign", s.authed(s.formAssignTask))
	s.mux.HandleFunc("POST /tasks/{id}/comments", s.authed(s.formAddComment))
	s.mux.HandleFunc("POST /comments/{id}/delete", s.authed(s.formDeleteComment))
	s.mux.HandleFunc("POST /tasks/{id}/delete", s.authed(s.formDeleteTask))
	s.mux.HandleFunc("POST /tasks/{id}/restore", s.authed(s.formRestoreTask))

	// JSON API (used by the CLI). Callers authenticate with
	// "Authorization: Bearer <token>"; without one only public lists are visible.
	s.mux.HandleFunc("POST /api/users", s.limited(s.apiCreateUser, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusTooManyRequests, "too many new people from this address; wait a minute")
	}))
	s.mux.HandleFunc("GET /api/me", s.apiMe)
	s.mux.HandleFunc("POST /api/join/{code}", s.apiJoin)
	s.mux.HandleFunc("GET /api/mine", s.apiMine)
	s.mux.HandleFunc("GET /api/lists", s.apiLists)
	s.mux.HandleFunc("POST /api/lists", s.apiCreateList)
	s.mux.HandleFunc("PATCH /api/lists/{id}", s.apiRenameList)
	s.mux.HandleFunc("DELETE /api/lists/{id}", s.apiDeleteList)
	s.mux.HandleFunc("POST /api/lists/{id}/restore", s.apiRestoreList)
	s.mux.HandleFunc("GET /api/lists/{id}/members", s.apiMembers)
	s.mux.HandleFunc("GET /api/lists/{id}/tasks", s.apiTasks)
	s.mux.HandleFunc("POST /api/lists/{id}/tasks", s.apiAddTask)
	s.mux.HandleFunc("PATCH /api/tasks/{id}", s.apiPatchTask)
	s.mux.HandleFunc("DELETE /api/tasks/{id}", s.apiDeleteTask)
	s.mux.HandleFunc("GET /api/tasks/{id}/comments", s.apiComments)
	s.mux.HandleFunc("POST /api/tasks/{id}/comments", s.apiAddComment)
	s.mux.HandleFunc("DELETE /api/comments/{id}", s.apiDeleteComment)
}

// ---- identity ---------------------------------------------------------

func tokenFrom(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value
	}
	return ""
}

func userFrom(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey{}).(*store.User)
	return u
}

// userID is the caller's ID, or 0 for an anonymous caller.
func userID(r *http.Request) int64 {
	if u := userFrom(r); u != nil {
		return u.ID
	}
	return 0
}

// isHTTPS reports whether the browser reached us over HTTPS, either directly
// or through a proxy that terminated TLS and said so. Trusting the header is
// safe for both of its uses: forging it can only make a cookie stricter or a
// link more secure than it needed to be, never less.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func setSession(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// The token is the whole account, so it must never travel over plain
		// HTTP. Behind a proxy that terminates TLS, r.TLS is always nil, which
		// used to leave this off in exactly the deployment it matters most.
		Secure: isHTTPS(r),
	})
}

// safeNext keeps post-login redirects on this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) {
		return "/"
	}
	return next
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

type userHandler func(http.ResponseWriter, *http.Request, *store.User)

// authed sends visitors without a name to the welcome page first.
func (s *Server) authed(h userHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := userFrom(r)
		if u == nil {
			dest := "/welcome"
			if r.Method == http.MethodGet {
				dest += "?next=" + url.QueryEscape(r.URL.RequestURI())
			}
			http.Redirect(w, r, dest, http.StatusSeeOther)
			return
		}
		h(w, r, u)
	}
}

// ---- welcome / join / profile ----------------------------------------

type simplePage struct {
	Title, Heading, Subtitle, Action, Next, Button, Error string
	NeedName, ShowToken                                   bool
	Demo                                                  bool
}

func (s *Server) renderTmpl(w http.ResponseWriter, status int, name string, data any) {
	if p, ok := data.(simplePage); ok {
		p.Demo = s.demoSeed != nil
		data = p
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func welcomePage(next, errMsg string) simplePage {
	return simplePage{
		Title:     "Welcome",
		Heading:   "Welcome to Doables",
		Subtitle:  "What should we call you? People you share lists with will see this name next to the tasks you add and finish.",
		Action:    "/welcome",
		Next:      safeNext(next),
		Button:    "Get started",
		Error:     errMsg,
		NeedName:  true,
		ShowToken: true,
	}
}

func (s *Server) welcomeGet(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if userFrom(r) != nil {
		http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
		return
	}
	s.renderTmpl(w, http.StatusOK, "welcome.html", welcomePage(next, ""))
}

// tooManySignups turns the sign-up form down without losing the page.
func (s *Server) tooManySignups(w http.ResponseWriter, r *http.Request) {
	s.renderTmpl(w, http.StatusTooManyRequests, "welcome.html",
		welcomePage(safeNext(r.FormValue("next")), "Too many new names from this connection. Try again in a minute."))
}

func (s *Server) welcomePost(w http.ResponseWriter, r *http.Request) {
	next := r.FormValue("next")
	u, token, err := s.store.CreateUser(r.FormValue("name"))
	if errors.Is(err, store.ErrInvalid) {
		s.renderTmpl(w, http.StatusBadRequest, "welcome.html", welcomePage(next, "Please enter a name (up to 40 characters)."))
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	setSession(w, r, token)
	if s.demoSeed != nil {
		// A demo that starts on an empty page shows nothing, so the visitor
		// begins inside a list that already has people and work in it,
		// unless they were on their way somewhere in particular.
		first, err := s.demoSeed(u)
		if err != nil {
			s.fail(w, err)
			return
		}
		if dest := safeNext(next); dest != "/" {
			http.Redirect(w, r, dest, http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, listPath(first), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
}

// welcomeToken signs in with an existing token, e.g. on a second device.
func (s *Server) welcomeToken(w http.ResponseWriter, r *http.Request) {
	next := r.FormValue("next")
	token := strings.TrimSpace(r.FormValue("token"))
	if _, err := s.store.UserByToken(token); err != nil {
		s.renderTmpl(w, http.StatusBadRequest, "welcome.html", welcomePage(next, "That token wasn't recognised."))
		return
	}
	setSession(w, r, token)
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
}

func joinPage(l store.List, code string, u *store.User, errMsg string) simplePage {
	p := simplePage{
		Title:    "Join " + l.Name,
		Heading:  "Join “" + l.Name + "”",
		Action:   "/join/" + code,
		Next:     "/join/" + code,
		Button:   "Join list",
		Error:    errMsg,
		NeedName: u == nil,
	}
	if u == nil {
		p.Subtitle = "You've been invited to share this list. Pick a name so the others can tell who's who."
		p.ShowToken = true
	} else {
		p.Subtitle = "You'll join as " + u.Name + " and can add tasks and tick them off with everyone else."
	}
	return p
}

func (s *Server) joinGet(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	l, err := s.store.ListByInvite(code)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	u := userFrom(r)
	if l.Public() || (u != nil && s.isMember(l.ID, u.ID)) {
		http.Redirect(w, r, listPath(l.ID), http.StatusSeeOther)
		return
	}
	s.renderTmpl(w, http.StatusOK, "welcome.html", joinPage(l, code, u, ""))
}

func (s *Server) joinPost(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	l, err := s.store.ListByInvite(code)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	if l.Public() {
		http.Redirect(w, r, listPath(l.ID), http.StatusSeeOther)
		return
	}
	u := userFrom(r)
	if u == nil {
		// Joining without an identity creates one, so it counts against the
		// same limit as signing up; otherwise one invite link would be a way
		// round it. Someone already signed in is not creating anybody.
		if !s.signups.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			s.renderTmpl(w, http.StatusTooManyRequests, "welcome.html",
				joinPage(l, code, nil, "Too many new names from this connection. Try again in a minute."))
			return
		}
		nu, token, err := s.store.CreateUser(r.FormValue("name"))
		if errors.Is(err, store.ErrInvalid) {
			s.renderTmpl(w, http.StatusBadRequest, "welcome.html", joinPage(l, code, nil, "Please enter a name (up to 40 characters)."))
			return
		} else if err != nil {
			s.fail(w, err)
			return
		}
		setSession(w, r, token)
		u = &nu
	}
	if err := s.store.AddMember(l.ID, u.ID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, listPath(l.ID), http.StatusSeeOther)
}

func (s *Server) isMember(listID, uid int64) bool {
	_, err := s.store.Access(listID, uid)
	return err == nil
}

// meGet used to be a page; the profile is now a dialog on every page, so old
// links and bookmarks just open it.
func (s *Server) meGet(w http.ResponseWriter, r *http.Request, u *store.User) {
	http.Redirect(w, r, "/#profile", http.StatusSeeOther)
}

// meToken hands the caller their own sign-in token. It is fetched on demand
// when they choose to reveal or copy it, rather than being written into every
// page, and it is never cached.
func (s *Server) meToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": tokenFrom(r)})
}

// meTokenSaved records that someone has their token somewhere safe. There is
// no way back from losing it, so until this is set every page says so.
func (s *Server) meTokenSaved(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := s.store.MarkTokenSaved(u.ID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, "/"), http.StatusSeeOther)
}

// mePost renames the caller. A blank or over-long name leaves it unchanged
// (the form's input already enforces both).
func (s *Server) mePost(w http.ResponseWriter, r *http.Request, u *store.User) {
	if err := s.store.RenameUser(u.ID, r.FormValue("name")); err != nil && !errors.Is(err, store.ErrInvalid) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, "/"), http.StatusSeeOther)
}

// ---- HTML UI ----------------------------------------------------------

type pageData struct {
	View         string // "dashboard", "list", "today" or "mine"
	User         *store.User
	Path         string // this page's URL, so actions can return here
	Today        string // today's date, YYYY-MM-DD
	TodayCount   int    // tasks overdue or due today, across all lists
	TodayOverdue bool   // some of those are overdue
	MineCount    int    // open tasks assigned to me, across all lists
	Demo         bool   // this server is a public demo
	Lists        []store.List
	Current      *store.List
	Tasks        []store.Task
	Groups       []taskGroup // Today view sections
	Filter       string      // "all", "open" or "done"
	Tag          string      // only tasks with this #tag are shown, when set
	TagCounts    []store.TagCount
	// Count is the tasks with the tag, if any, for the All/Open/Done tabs.
	Count     struct{ All, Open, Done int }
	IsOwner   bool // may delete the list and manage its members
	Members   []store.Member
	InviteURL string
	Threads   map[int64][]store.Comment // comments by task, on a list's own page
}

// taskGroup is one section of the Today view.
type taskGroup struct {
	Title, Icon, Color string
	Tasks              []store.Task
}

// basePage fills in what every page with the sidebar needs.
func (s *Server) basePage(r *http.Request, u *store.User, view string) (pageData, error) {
	d := pageData{View: view, User: u, Path: r.URL.RequestURI(), Today: today(), Demo: s.demoSeed != nil}
	var err error
	if d.Lists, err = s.store.Lists(u.ID); err != nil {
		return d, err
	}
	overdue, due, err := s.store.DueCounts(u.ID, d.Today)
	if err != nil {
		return d, err
	}
	d.TodayCount, d.TodayOverdue = overdue+due, overdue > 0
	d.MineCount, err = s.store.AssignedCount(u.ID)
	return d, err
}

// backTo is where an action should send the browser: the page it came from
// (the form's "next" field), or fallback.
func backTo(r *http.Request, fallback string) string {
	if next := r.FormValue("next"); next != "" {
		return safeNext(next)
	}
	return fallback
}

func (s *Server) pageToday(w http.ResponseWriter, r *http.Request, u *store.User) {
	d, err := s.basePage(r, u, "today")
	if err != nil {
		s.fail(w, err)
		return
	}
	now, _ := parseDate(d.Today)
	tasks, err := s.store.DueTasks(u.ID, now.AddDate(0, 0, 7).Format(dateLayout))
	if err != nil {
		s.fail(w, err)
		return
	}
	overdue := taskGroup{Title: "Overdue", Icon: "alert-triangle", Color: "red"}
	dueToday := taskGroup{Title: "Today", Icon: "calendar-event", Color: "orange"}
	upcoming := taskGroup{Title: "Next 7 days", Icon: "calendar-week", Color: "blue"}
	for _, t := range tasks {
		switch {
		case t.DueDate < d.Today:
			overdue.Tasks = append(overdue.Tasks, t)
		case t.DueDate == d.Today:
			dueToday.Tasks = append(dueToday.Tasks, t)
		default:
			upcoming.Tasks = append(upcoming.Tasks, t)
		}
	}
	for _, g := range []taskGroup{overdue, dueToday, upcoming} {
		if len(g.Tasks) > 0 {
			d.Groups = append(d.Groups, g)
		}
	}
	s.renderTmpl(w, http.StatusOK, "page.html", d)
}

// pageMine is everything that is this person's job, across every list.
func (s *Server) pageMine(w http.ResponseWriter, r *http.Request, u *store.User) {
	d, err := s.basePage(r, u, "mine")
	if err != nil {
		s.fail(w, err)
		return
	}
	if d.Tasks, err = s.store.AssignedTasks(u.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.renderTmpl(w, http.StatusOK, "page.html", d)
}

func listPath(id int64) string { return "/lists/" + strconv.FormatInt(id, 10) }

// canManage reports whether userID may delete a list or manage its members:
// its owner, or anyone for a public list.
func canManage(l store.List, userID int64) bool { return l.Public() || l.OwnerID == userID }

func (s *Server) pageIndex(w http.ResponseWriter, r *http.Request, u *store.User) {
	d, err := s.basePage(r, u, "dashboard")
	if err != nil {
		s.fail(w, err)
		return
	}
	s.renderTmpl(w, http.StatusOK, "page.html", d)
}

func (s *Server) pageList(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	cur, err := s.store.Access(id, u.ID)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	d, err := s.basePage(r, u, "list")
	if err != nil {
		s.fail(w, err)
		return
	}
	tasks, err := s.store.Tasks(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	filter := r.URL.Query().Get("filter")
	if filter != "open" && filter != "done" {
		filter = "all"
	}
	// The tags come from every task, so the row of them above the list does
	// not shrink to the one tag being shown.
	d.TagCounts = store.CountTags(tasks)
	if tag, ok := store.NormalizeTag(r.URL.Query().Get("tag")); ok {
		d.Tag, tasks = tag, store.WithTag(tasks, tag)
	}
	for _, t := range tasks {
		if t.Done {
			d.Count.Done++
		} else {
			d.Count.Open++
		}
	}
	d.Count.All = len(tasks)
	if filter != "all" {
		kept := tasks[:0]
		for _, t := range tasks {
			if t.Done == (filter == "done") {
				kept = append(kept, t)
			}
		}
		tasks = kept
	}
	d.Current, d.Tasks, d.Filter, d.IsOwner = &cur, tasks, filter, canManage(cur, u.ID)
	if d.Threads, err = s.store.ListComments(id); err != nil {
		s.fail(w, err)
		return
	}
	if !cur.Public() {
		if d.Members, err = s.store.Members(id); err != nil {
			s.fail(w, err)
			return
		}
		d.InviteURL = baseURL(r) + "/join/" + cur.InviteCode
	}
	s.renderTmpl(w, http.StatusOK, "page.html", d)
}

func (s *Server) formCreateList(w http.ResponseWriter, r *http.Request, u *store.User) {
	l, err := s.store.CreateList(r.FormValue("name"), u.ID)
	if errors.Is(err, store.ErrInvalid) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, listPath(l.ID), http.StatusSeeOther)
}

// manageList loads a list for an action only its owner may take.
func (s *Server) manageList(w http.ResponseWriter, r *http.Request, u *store.User) (store.List, bool) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return store.List{}, false
	}
	l, err := s.store.Access(id, u.ID)
	if err != nil {
		s.htmlErr(w, r, err)
		return l, false
	}
	if !canManage(l, u.ID) {
		http.Error(w, "only the list's owner can do that", http.StatusForbidden)
		return l, false
	}
	return l, true
}

func (s *Server) formDeleteList(w http.ResponseWriter, r *http.Request, u *store.User) {
	l, ok := s.manageList(w, r, u)
	if !ok {
		return
	}
	if err := s.store.DeleteList(l.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	// Deleting a list takes the browser somewhere else, so the offer to undo
	// has to survive the journey: the dashboard turns this into a toast.
	http.Redirect(w, r, "/?undo="+strconv.FormatInt(l.ID, 10), http.StatusSeeOther)
}

// formRestoreList takes a list back out of the trash.
func (s *Server) formRestoreList(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	l, err := s.store.RestoreList(id, u.ID)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, listPath(l.ID), http.StatusSeeOther)
}

func (s *Server) formClaimList(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	l, err := s.store.Access(id, u.ID)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	if l.Public() {
		if err := s.store.ClaimList(id, u.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.fail(w, err)
			return
		}
	}
	http.Redirect(w, r, listPath(id)+"#share", http.StatusSeeOther)
}

func (s *Server) formResetInvite(w http.ResponseWriter, r *http.Request, u *store.User) {
	l, ok := s.manageList(w, r, u)
	if !ok {
		return
	}
	if _, err := s.store.ResetInvite(l.ID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, listPath(l.ID)+"#share", http.StatusSeeOther)
}

func (s *Server) formLeaveList(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.Access(id, u.ID); err != nil {
		s.htmlErr(w, r, err)
		return
	}
	// The owner cannot leave (RemoveMember refuses); they delete the list instead.
	if err := s.store.RemoveMember(id, u.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) formRemoveMember(w http.ResponseWriter, r *http.Request, u *store.User) {
	l, ok := s.manageList(w, r, u)
	if !ok {
		return
	}
	target, ok := pathInt(r, "uid")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.RemoveMember(l.ID, target); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, listPath(l.ID)+"#share", http.StatusSeeOther)
}

func (s *Server) formAddTask(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.Access(id, u.ID); err != nil {
		s.htmlErr(w, r, err)
		return
	}
	title := r.FormValue("title")
	// A task added while the list is showing only #shopping would vanish
	// the moment it was added, so it is tagged #shopping too.
	if tag, ok := store.NormalizeTag(r.FormValue("tag")); ok && strings.TrimSpace(title) != "" {
		if !slices.Contains(store.TagsIn(title, r.FormValue("description")), tag) {
			title = strings.TrimRight(title, " ") + " #" + tag
		}
	}
	_, err := s.store.AddTask(id, u.ID, title, r.FormValue("description"), r.FormValue("due"))
	if err != nil && !errors.Is(err, store.ErrInvalid) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, listPath(id)), http.StatusSeeOther)
}

func (s *Server) formRenameList(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.Access(id, u.ID); err != nil {
		s.htmlErr(w, r, err)
		return
	}
	if err := s.store.RenameList(id, r.FormValue("name")); err != nil && !errors.Is(err, store.ErrInvalid) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, listPath(id)), http.StatusSeeOther)
}

func (s *Server) formEditTask(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	t, _, err := s.store.TaskAccess(id, u.ID)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	title, desc, due := r.FormValue("title"), r.FormValue("description"), r.FormValue("due")
	_, err = s.store.UpdateTask(id, store.TaskUpdate{Title: &title, Description: &desc, DueDate: &due})
	if err != nil && !errors.Is(err, store.ErrInvalid) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, listPath(t.ListID)), http.StatusSeeOther)
}

// formAssignTask makes a task somebody's job. A blank "user" means nobody's.
// Someone who is not on the list is refused by the store, and the page simply
// comes back unchanged.
func (s *Server) formAssignTask(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	t, _, err := s.store.TaskAccess(id, u.ID)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	var assignee int64
	if v := r.FormValue("user"); v != "" {
		if assignee, err = strconv.ParseInt(v, 10, 64); err != nil {
			http.Error(w, "invalid user", http.StatusBadRequest)
			return
		}
	}
	if _, err := s.store.AssignTask(id, assignee); err != nil && !errors.Is(err, store.ErrInvalid) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, listPath(t.ListID)), http.StatusSeeOther)
}

// formAddComment adds to a task's conversation.
func (s *Server) formAddComment(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	t, _, err := s.store.TaskAccess(id, u.ID)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	if _, err := s.store.AddComment(id, u.ID, r.FormValue("body")); err != nil && !errors.Is(err, store.ErrInvalid) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, listPath(t.ListID)), http.StatusSeeOther)
}

// formDeleteComment takes back something you said.
func (s *Server) formDeleteComment(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	c, err := s.store.CommentAccess(id, u.ID)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	if err := s.store.DeleteComment(id, u.ID); errors.Is(err, store.ErrForbidden) {
		http.Error(w, "only the person who wrote a comment can delete it", http.StatusForbidden)
		return
	} else if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	t, _ := s.store.Task(c.TaskID)
	http.Redirect(w, r, backTo(r, listPath(t.ListID)), http.StatusSeeOther)
}

func (s *Server) formRestoreTask(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	t, err := s.store.RestoreTask(id, u.ID)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	http.Redirect(w, r, backTo(r, listPath(t.ListID)), http.StatusSeeOther)
}

func (s *Server) formToggleTask(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	t, _, err := s.store.TaskAccess(id, u.ID)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	if _, err := s.store.SetDone(id, !t.Done, u.ID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, listPath(t.ListID)), http.StatusSeeOther)
}

func (s *Server) formDeleteTask(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	t, _, err := s.store.TaskAccess(id, u.ID)
	if err != nil {
		s.htmlErr(w, r, err)
		return
	}
	if err := s.store.DeleteTask(id); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, backTo(r, listPath(t.ListID)), http.StatusSeeOther)
}

// ---- JSON API ---------------------------------------------------------

func (s *Server) apiCreateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	u, token, err := s.store.CreateUser(in.Name)
	s.respond(w, http.StatusCreated, struct {
		store.User
		Token string `json:"token"`
	}{u, token}, err)
}

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u == nil {
		writeError(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}
	s.respond(w, http.StatusOK, u, nil)
}

func (s *Server) apiJoin(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u == nil {
		writeError(w, http.StatusUnauthorized, "missing or invalid token")
		return
	}
	l, err := s.store.ListByInvite(r.PathValue("code"))
	if err == nil && !l.Public() {
		if err = s.store.AddMember(l.ID, u.ID); err == nil {
			l, err = s.store.Access(l.ID, u.ID)
		}
	}
	s.respond(w, http.StatusOK, l, err)
}

// apiMine lists the open tasks assigned to the caller.
func (s *Server) apiMine(w http.ResponseWriter, r *http.Request) {
	if userID(r) == 0 {
		writeError(w, http.StatusUnauthorized, "sign in first: send Authorization: Bearer <token>")
		return
	}
	ts, err := s.store.AssignedTasks(userID(r))
	s.respond(w, http.StatusOK, ts, err)
}

func (s *Server) apiLists(w http.ResponseWriter, r *http.Request) {
	lists, err := s.store.Lists(userID(r))
	s.respond(w, http.StatusOK, lists, err)
}

func (s *Server) apiCreateList(w http.ResponseWriter, r *http.Request) {
	// A list made without a token would belong to nobody, which means everyone
	// who can reach this server can read and edit it. That is how the public
	// lists in older databases came about, and it surprises people. Old ones
	// still work; new ones are not made by accident.
	if userID(r) == 0 {
		writeError(w, http.StatusUnauthorized,
			`a list needs an owner: run "doables register <name>" and use the token it prints`)
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	l, err := s.store.CreateList(in.Name, userID(r))
	s.respond(w, http.StatusCreated, l, err)
}

func (s *Server) apiDeleteList(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	l, err := s.store.Access(id, userID(r))
	if err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	if !canManage(l, userID(r)) {
		writeError(w, http.StatusForbidden, "only the list's owner can delete it")
		return
	}
	if err := s.store.DeleteList(id); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiRestoreList takes a list back out of the trash, for the day it stays there.
func (s *Server) apiRestoreList(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	l, err := s.store.RestoreList(id, userID(r))
	s.respond(w, http.StatusOK, l, err)
}

func (s *Server) apiMembers(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := s.store.Access(id, userID(r)); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	members, err := s.store.Members(id)
	s.respond(w, http.StatusOK, members, err)
}

func (s *Server) apiTasks(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := s.store.Access(id, userID(r)); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	var tag string
	if q := r.URL.Query().Get("tag"); q != "" {
		var ok bool
		if tag, ok = store.NormalizeTag(q); !ok {
			writeError(w, http.StatusBadRequest, "invalid tag: a tag is one word with a letter in it, like #shopping")
			return
		}
	}
	tasks, err := s.store.Tasks(id)
	if tag != "" {
		tasks = store.WithTag(tasks, tag)
	}
	s.respond(w, http.StatusOK, tasks, err)
}

func (s *Server) apiAddTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		DueDate     string `json:"due_date"`
	}
	if !decode(w, r, &in) {
		return
	}
	if _, err := s.store.Access(id, userID(r)); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	t, err := s.store.AddTask(id, userID(r), in.Title, in.Description, in.DueDate)
	s.respond(w, http.StatusCreated, t, err)
}

// apiPatchTask edits a task. Every field is optional but at least one is
// required; an empty "due_date" clears the due date.
func (s *Server) apiPatchTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in struct {
		Done        *bool   `json:"done"`
		Title       *string `json:"title"`
		Description *string `json:"description"`
		DueDate     *string `json:"due_date"`
		AssigneeID  *int64  `json:"assignee_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Done == nil && in.Title == nil && in.Description == nil && in.DueDate == nil && in.AssigneeID == nil {
		writeError(w, http.StatusBadRequest, `provide at least one of "done", "title", "description", "due_date", "assignee_id"`)
		return
	}
	if _, _, err := s.store.TaskAccess(id, userID(r)); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	var t store.Task
	var err error
	if in.Title != nil || in.Description != nil || in.DueDate != nil {
		t, err = s.store.UpdateTask(id, store.TaskUpdate{Title: in.Title, Description: in.Description, DueDate: in.DueDate})
	}
	if err == nil && in.AssigneeID != nil {
		t, err = s.store.AssignTask(id, *in.AssigneeID)
		if errors.Is(err, store.ErrInvalid) {
			writeError(w, http.StatusBadRequest, "that person is not on this list")
			return
		}
	}
	if err == nil && in.Done != nil {
		t, err = s.store.SetDone(id, *in.Done, userID(r))
	}
	s.respond(w, http.StatusOK, t, err)
}

func (s *Server) apiComments(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, _, err := s.store.TaskAccess(id, userID(r)); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	cs, err := s.store.Comments(id)
	s.respond(w, http.StatusOK, cs, err)
}

func (s *Server) apiAddComment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	// A comment nobody wrote could never be taken back, and says nothing
	// about who thinks so, so this is one thing a public list does not let
	// anonymous callers do.
	if userID(r) == 0 {
		writeError(w, http.StatusUnauthorized, "a comment needs an author: run \"doables register <name>\" and use the token it prints")
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if !decode(w, r, &in) {
		return
	}
	if _, _, err := s.store.TaskAccess(id, userID(r)); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	c, err := s.store.AddComment(id, userID(r), in.Body)
	s.respond(w, http.StatusCreated, c, err)
}

func (s *Server) apiDeleteComment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := s.store.CommentAccess(id, userID(r)); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	if err := s.store.DeleteComment(id, userID(r)); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiRenameList(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	if _, err := s.store.Access(id, userID(r)); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	if err := s.store.RenameList(id, in.Name); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	l, err := s.store.Access(id, userID(r))
	s.respond(w, http.StatusOK, l, err)
}

func (s *Server) apiDeleteTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, _, err := s.store.TaskAccess(id, userID(r)); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	if err := s.store.DeleteTask(id); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- helpers ----------------------------------------------------------

func pathInt(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id, err == nil && id > 0
}

func pathID(r *http.Request) (int64, bool) { return pathInt(r, "id") }

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// respond writes v as JSON with the given status, or maps err to an HTTP error.
func (s *Server) respond(w http.ResponseWriter, status int, v any, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid input: names and titles must not be empty (or too long), dates must be YYYY-MM-DD")
	case errors.Is(err, store.ErrForbidden):
		writeError(w, http.StatusForbidden, "only the person who wrote it can do that")
	case err != nil:
		log.Printf("api: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// htmlErr maps a store error to a plain HTTP error page.
func (s *Server) htmlErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	s.fail(w, err)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	log.Printf("web: %v", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
