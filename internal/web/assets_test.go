package web

import (
	"io/fs"
	"strings"
	"testing"
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
