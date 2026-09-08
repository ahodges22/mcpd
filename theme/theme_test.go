package theme

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var embedded = []string{"theme.css", "UbuntuSans.woff2", "UbuntuSansMono.woff2", "LICENCE-ubuntu-font.txt"}

func TestEmbeddedFilesArePresent(t *testing.T) {
	if Version <= 0 {
		t.Errorf("Version = %d, want > 0", Version)
	}
	for _, name := range embedded {
		raw, err := fs.ReadFile(FS(), name)
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if len(raw) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
	licence, _ := fs.ReadFile(FS(), "LICENCE-ubuntu-font.txt")
	if !strings.Contains(string(licence), "UBUNTU FONT LICENCE") {
		t.Error("the licence file does not carry the Ubuntu Font Licence")
	}
}

func TestThemeCSSContract(t *testing.T) {
	raw, err := fs.ReadFile(FS(), "theme.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)
	if strings.Contains(css, "url(http") {
		t.Error("theme.css loads a remote URL; fonts must be relative so any mount prefix works")
	}
	for _, token := range []string{"--up", "--wait", "--fault", "--off", "--cold"} {
		if !strings.Contains(css, token+":") {
			t.Errorf("theme.css does not define %s", token)
		}
	}
}

func TestHandler(t *testing.T) {
	srv := httptest.NewServer(http.StripPrefix("/theme", Handler()))
	defer srv.Close()

	get := func(path string) *http.Response {
		t.Helper()
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}

	css := get("/theme/theme.css")
	if css.StatusCode != http.StatusOK || css.Header.Get("Content-Type") != "text/css; charset=utf-8" {
		t.Errorf("theme.css = %d %q", css.StatusCode, css.Header.Get("Content-Type"))
	}
	if cc := css.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", cc)
	}
	if css.Header.Get("ETag") == "" {
		t.Error("no ETag")
	}
	if font := get("/theme/UbuntuSans.woff2"); font.StatusCode != http.StatusOK || font.Header.Get("Content-Type") != "font/woff2" {
		t.Errorf("UbuntuSans.woff2 = %d %q", font.StatusCode, font.Header.Get("Content-Type"))
	}
	if res := get("/theme/nope.css"); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown name = %d, want 404", res.StatusCode)
	}
	res, err := http.Post(srv.URL+"/theme/theme.css", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", res.StatusCode)
	}
}
