package web_test

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"koupa/app/config"
	"koupa/app/server"
	"koupa/app/web"
)

func get(t *testing.T, path string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	s := server.New(config.Config{Version: "v2"}, server.Deps{Pages: web.Pages()})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, _ := io.ReadAll(rec.Body)
	return rec, string(body)
}

func TestRoutesServePages(t *testing.T) {
	for path, marker := range map[string]string{
		"/":        `data-page="order"`,
		"/kitchen": `data-page="kitchen"`,
		"/board":   `data-page="board"`,
		"/app.js":  "initBoard",
		"/app.css": "--primary",
	} {
		rec, body := get(t, path)
		if rec.Code != 200 || !strings.Contains(body, marker) {
			t.Errorf("%s: status %d, marker %q missing", path, rec.Code, marker)
		}
	}
}

// All three pages share one header: brand, the new slogan and the nav with
// the current page marked. The course tagline is not part of the app UI.
func TestPagesShareHeader(t *testing.T) {
	for path, current := range map[string]string{
		"/":        `<a href="/" aria-current="page">點餐</a>`,
		"/kitchen": `<a href="/kitchen" aria-current="page">廚房</a>`,
		"/board":   `<a href="/board" aria-current="page">叫號看板</a>`,
	} {
		_, body := get(t, path)
		for _, want := range []string{"Koupa 酷杯", "一杯好茶，隨時都在", current, `href="/app.css"`, `id="debug"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: %q missing", path, want)
			}
		}
		if strings.Contains(body, "零停機") {
			t.Errorf("%s: still shows the old slogan", path)
		}
		if strings.Count(body, `aria-current="page"`) != 1 {
			t.Errorf("%s: want exactly one current nav link", path)
		}
		// No external fonts or scripts: the pages must work offline.
		if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
			t.Errorf("%s: references an external URL", path)
		}
	}
}

func TestImages(t *testing.T) {
	_, js := get(t, "/app.js")
	for _, name := range []string{"boba-milk-tea", "sunset-black-tea", "green-tea-trio", "winter-melon-lemon"} {
		rec, body := get(t, "/img/"+name+".webp")
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", name, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/webp" {
			t.Errorf("%s: Content-Type %q", name, ct)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=") {
			t.Errorf("%s: Cache-Control %q", name, cc)
		}
		if !strings.HasPrefix(body, "RIFF") || body[8:12] != "WEBP" {
			t.Errorf("%s: body is not a WebP file", name)
		}
		if !strings.Contains(js, "'"+name+"'") {
			t.Errorf("%s: not mapped in app.js", name)
		}
	}
	for _, path := range []string{"/img/nope.webp", "/img/CREDITS.md", "/img/", "/img/..%2fapp.js", "/img/boba-milk-tea.png"} {
		if rec, _ := get(t, path); rec.Code != 404 {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
		}
	}
}

// Every embedded photo stays small enough for the distroless image.
func TestImageSizes(t *testing.T) {
	matches, err := fs.Glob(web.Pages(), "img/*.webp")
	if err != nil || len(matches) != 4 {
		t.Fatalf("img/*.webp = %v, %v; want 4 files", matches, err)
	}
	for _, m := range matches {
		st, err := fs.Stat(web.Pages(), m)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() > 150<<10 {
			t.Errorf("%s: %d bytes, limit 150 KiB", m, st.Size())
		}
	}
}
