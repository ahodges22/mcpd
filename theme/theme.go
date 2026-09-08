// Package theme is the shared visual language for mcpd and the tools built on it:
// design tokens, primitive classes, and the two Ubuntu Sans font subsets, served from
// one directory so theme.css can load the fonts by relative URL under any mount prefix.
//
// A consumer mounts Handler under a prefix, links <prefix>/theme.css before its own
// stylesheet, and builds its page layout on the primitives.
package theme

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
)

// Version identifies the token set and primitive class contracts. Bump it when either
// changes incompatibly; a consumer asserts a minimum at compile time with a constant
// expression such as `const _ = theme.Version - 1`.
const Version = 1

//go:embed theme.css UbuntuSans.woff2 UbuntuSansMono.woff2 LICENCE-ubuntu-font.txt
var files embed.FS

// FS exposes the embedded files by bare name for consumers that serve them themselves.
func FS() fs.FS { return files }

// contentTypes is the fixed set of served files. A name outside it is a 404.
var contentTypes = map[string]string{
	"theme.css":               "text/css; charset=utf-8",
	"UbuntuSans.woff2":        "font/woff2",
	"UbuntuSansMono.woff2":    "font/woff2",
	"LICENCE-ubuntu-font.txt": "text/plain; charset=utf-8",
}

// Handler serves the four files by the last path element, so it works mounted under any
// prefix without StripPrefix. The URLs do not change between releases, so the response
// is cacheable but must revalidate: the ETag is Version plus a content hash, and a daemon
// upgrade that changes a file answers the next conditional request with the new bytes.
func Handler() http.Handler {
	etags := make(map[string]string, len(contentTypes))
	for name := range contentTypes {
		raw, err := files.ReadFile(name)
		if err != nil {
			panic("theme: embedded file missing: " + name)
		}
		sum := sha256.Sum256(raw)
		etags[name] = fmt.Sprintf(`"v%d-%s"`, Version, hex.EncodeToString(sum[:8]))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := path.Base(r.URL.Path)
		ct, ok := contentTypes[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", etags[name])
		if r.Header.Get("If-None-Match") == etags[name] {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		raw, _ := files.ReadFile(name)
		h.Set("Content-Type", ct)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Length", fmt.Sprint(len(raw)))
		if r.Method == http.MethodHead {
			return
		}
		w.Write(raw)
	})
}
