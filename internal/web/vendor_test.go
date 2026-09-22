package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// templateSource returns every template, concatenated.
func templateSource(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates found: %v", err)
	}
	for _, name := range files {
		src, err := templateFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(src)
	}
	return b.String()
}

// The icon font is cut down to the icons the templates ask for, which is what
// keeps it at 8KB rather than 844KB. Using a new icon without regenerating it
// would render a blank square, and nothing else would complain.
func TestEveryIconIsVendored(t *testing.T) {
	css, err := staticFS.ReadFile("static/vendor/tabler-icons.css")
	if err != nil {
		t.Fatal(err)
	}
	names := regexp.MustCompile(`\bti-[a-z0-9-]+`).FindAllString(templateSource(t), -1)
	if len(names) < 30 {
		t.Fatalf("only found %d icon uses; the search is probably broken", len(names))
	}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		if !strings.Contains(string(css), "."+name+":before") {
			t.Errorf("%s is used in a template but is not in the subset font.\n"+
				"Run: python tools/vendor/fetch.py", name)
		}
	}
}

// Doables is meant to be run by the person using it, so a page must not fetch
// anything from anywhere else: a CDN would learn the address of everyone using
// the app, the app would stop working on a network with no way out, and
// whoever controls that CDN could run code on the same origin as the session
// cookie.
func TestPagesFetchNothingFromAnywhereElse(t *testing.T) {
	refs := regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(templateSource(t), -1)
	if len(refs) < 5 {
		t.Fatalf("only found %d references; the search is probably broken", len(refs))
	}
	for _, m := range refs {
		url := m[1]
		// A data: URL carries its own content, so it fetches nothing. The one
		// in use is an SVG favicon, whose xmlns is a name, not an address.
		if strings.HasPrefix(url, "data:") {
			continue
		}
		if strings.Contains(url, "//") {
			t.Errorf("template fetches %q from another origin; vendor it with tools/vendor/fetch.py", url)
		}
	}
}

// The policy only means anything while every asset really is local.
func TestPolicyKeepsThePageToItsOwnOrigin(t *testing.T) {
	e := newEnv(t)
	alice := e.register("Alice")
	_, body := e.page(alice, "/")
	if body == "" {
		t.Fatal("no page came back")
	}
	for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("the policy is missing %q", want)
		}
	}
}
