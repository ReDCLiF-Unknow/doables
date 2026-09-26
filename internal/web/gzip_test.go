package web

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fetch makes a request with exactly the headers given. Setting
// Accept-Encoding ourselves stops Go's client from quietly asking for gzip
// and undoing it, so the test sees what a browser would receive.
func (e *env) fetch(method, path, token string, headers map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	}
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Errorf("%s %s: reading the body: %v", method, path, err)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" && cl != itoa(int64(len(body))) {
		e.t.Errorf("%s %s: Content-Length is %s, the body %d bytes", method, path, cl, len(body))
	}
	return resp, body
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("broken gzip: %v", err)
	}
	return string(out)
}

func TestPagesAreCompressed(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	l := e.newList(alex, "Weekly shop")
	for i := 0; i < 40; i++ {
		e.call("POST", "/api/lists/"+itoa(l.ID)+"/tasks", alex, `{"title":"Something to buy #food"}`, nil)
	}
	listPage := "/lists/" + itoa(l.ID)

	resp, plain := e.fetch("GET", listPage, alex, nil)
	if resp.Header.Get("Content-Encoding") != "" || !strings.Contains(string(plain), "Weekly shop") {
		t.Fatal("a browser that did not ask for gzip got something other than the page")
	}
	resp, packed := e.fetch("GET", listPage, alex, map[string]string{"Accept-Encoding": "gzip, deflate, br"})
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("the list page was not compressed: %v", resp.Header)
	}
	if !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
		t.Error("a compressed page does not say it varies with Accept-Encoding")
	}
	if got := gunzip(t, packed); got != string(plain) {
		t.Error("the compressed page is not the same page")
	}
	if len(packed)*5 > len(plain) {
		t.Errorf("the page only shrank from %d to %d bytes", len(plain), len(packed))
	}
	t.Logf("list page with 40 tasks: %d KB, %d KB compressed", len(plain)/1024, len(packed)/1024)

	// The same for the API, the stylesheet and a redirect.
	for _, path := range []string{"/api/lists/" + itoa(l.ID) + "/tasks", "/static/vendor/tabler.min.css"} {
		if resp, body := e.fetch("GET", path, alex, map[string]string{"Accept-Encoding": "gzip"}); resp.Header.Get("Content-Encoding") != "gzip" {
			t.Errorf("%s was not compressed", path)
		} else {
			gunzip(t, body)
		}
	}
	resp, _ = e.fetch("GET", "/", "", map[string]string{"Accept-Encoding": "gzip"})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") == "" {
		t.Errorf("a redirect through gzip came back as %d", resp.StatusCode)
	}
	// Refusing it explicitly is respected.
	if resp, _ := e.fetch("GET", listPage, alex, map[string]string{"Accept-Encoding": "gzip;q=0, identity"}); resp.Header.Get("Content-Encoding") != "" {
		t.Error("gzip;q=0 still got gzip")
	}
}

func TestSomeThingsAreNotCompressed(t *testing.T) {
	e := newEnv(t)
	alex := e.register("Alex")
	gz := map[string]string{"Accept-Encoding": "gzip"}

	// Already compressed.
	for _, path := range []string{"/static/vendor/tabler-icons.woff2", "/static/icon-192.png"} {
		if resp, _ := e.fetch("GET", path, "", gz); resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != "" {
			t.Errorf("%s: status %d, encoding %q; want 200 and left alone", path, resp.StatusCode, resp.Header.Get("Content-Encoding"))
		}
	}
	// Part of a file: the range refers to the file as it is.
	resp, body := e.fetch("GET", "/static/vendor/tabler.min.css", "", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-9"})
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Encoding") != "" || len(body) != 10 {
		t.Errorf("a range request got status %d, encoding %q, %d bytes", resp.StatusCode, resp.Header.Get("Content-Encoding"), len(body))
	}

	// The live-update stream arrives as it is written, uncompressed.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/events", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: alex})
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Encoding") != "" || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("the event stream came back %q, %q", res.Header.Get("Content-Encoding"), res.Header.Get("Content-Type"))
	}
	first := make([]byte, 1)
	if _, err := res.Body.Read(first); err != nil {
		t.Errorf("nothing arrived on the event stream straight away: %v", err)
	}
}
