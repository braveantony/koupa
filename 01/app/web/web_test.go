package web_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"koupa/app/config"
	"koupa/app/server"
	"koupa/app/web"
)

func TestRoutesServePages(t *testing.T) {
	s := server.New(config.Config{Version: "v2"}, server.Deps{Pages: web.Pages()})
	for path, marker := range map[string]string{
		"/":        `data-page="order"`,
		"/kitchen": `data-page="kitchen"`,
		"/board":   `data-page="board"`,
		"/app.js":  "initBoard",
	} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		body, _ := io.ReadAll(rec.Body)
		if rec.Code != 200 || !strings.Contains(string(body), marker) {
			t.Errorf("%s: status %d, marker %q missing", path, rec.Code, marker)
		}
	}
}
