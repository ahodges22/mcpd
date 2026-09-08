package web

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/ahodges22/mcpd/theme"
)

// TestNoMarkupInsertionAPIInTheAssets is a regression gate rather than a one-time
// check: every backend-derived string, tool results most of all, reaches the DOM
// through textContent. A one-off grep would not survive the next edit. This test owns
// the DOM half of "a malicious tool result is inert"; the transport half belongs to
// TestAMaliciousToolResultIsCarriedAsEscapedJSON.
func TestNoMarkupInsertionAPIInTheAssets(t *testing.T) {
	banned := []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function("}

	app, err := fs.ReadFile(assetFS, "assets/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	// The sink is asserted positively as well as by the ban below, so deleting the
	// helper is as visible as changing what it assigns to.
	if !strings.Contains(string(app), "el.textContent = text;") {
		t.Error("app.js has no textContent sink: the single insertion point is gone or renamed")
	}

	for _, tree := range []fs.FS{assetFS, templateFS} {
		err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			raw, err := fs.ReadFile(tree, path)
			if err != nil {
				return err
			}
			body := string(raw)
			for _, api := range banned {
				if strings.Contains(body, api) {
					t.Errorf("%s uses %s: a backend-derived string could be inserted as markup", path, api)
				}
			}
			if strings.HasSuffix(path, ".html") {
				assertNoInlineCode(t, path, body)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk assets: %v", err)
		}
	}
}

func TestSecretFormUsesWriteOnlyPOST(t *testing.T) {
	app, err := fs.ReadFile(assetFS, "assets/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	body := string(app)
	for _, want := range []string{
		`document.querySelectorAll("form.secret-set-form")`,
		`form.addEventListener("submit"`,
		`event.preventDefault();`,
		`input.value = "";`,
		`post("/api/secrets/" + encodeURIComponent(form.dataset.secretName), { value: value })`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("app.js does not implement the write-only secret form behavior %q", want)
		}
	}
}

func TestEveryPageLinksTheFavicon(t *testing.T) {
	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(templateFS, path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(raw), `<link rel="icon" type="image/svg+xml" href="/assets/logo.svg">`) {
			t.Errorf("%s does not link the favicon", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}
}

// assertNoInlineCode keeps script and style in the asset files. Inline script would
// be invisible to the ban above, which is what would make this gate unsound.
func assertNoInlineCode(t *testing.T, path, body string) {
	t.Helper()
	if strings.Contains(body, "<style") {
		t.Errorf("%s carries an inline style block", path)
	}
	for _, after := range strings.Split(body, "<script")[1:] {
		if !strings.HasPrefix(after, " src=") {
			t.Errorf("%s carries an inline script block", path)
		}
	}
}

// TestPanelStylesheetRedefinesNoThemeToken keeps the two stylesheets in their roles:
// theme.css owns every design token, and style.css only lays the page out. A token
// redefined here would silently fork the shared look.
func TestPanelStylesheetRedefinesNoThemeToken(t *testing.T) {
	themeCSS, err := fs.ReadFile(theme.FS(), "theme.css")
	if err != nil {
		t.Fatalf("read theme.css: %v", err)
	}
	panelCSS, err := fs.ReadFile(assetFS, "assets/style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	tokens := rootTokens(string(themeCSS))
	if len(tokens) == 0 {
		t.Fatal("theme.css defines no :root tokens; the extraction is broken")
	}
	for _, token := range tokens {
		if strings.Contains(string(panelCSS), token+":") {
			t.Errorf("style.css redefines %s, which theme.css owns", token)
		}
	}
}

// rootTokens lists the custom property names declared inside the :root block.
func rootTokens(css string) []string {
	_, after, ok := strings.Cut(css, ":root {")
	if !ok {
		return nil
	}
	block, _, _ := strings.Cut(after, "}")
	var out []string
	for _, line := range strings.Split(block, "\n") {
		name, _, found := strings.Cut(strings.TrimSpace(line), ":")
		if found && strings.HasPrefix(name, "--") {
			out = append(out, name)
		}
	}
	return out
}
