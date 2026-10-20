package main

import (
	"crypto/rand"
	"embed"
	"io/fs"
	"net/http"
	"os"
	"strings"

	spinhttp "github.com/spinframework/spin-go-sdk/v3/http"

	"github.com/inertia-go/inertia-go"
	"github.com/inertia-go/inertia-go/session"
	"github.com/inertia-go/inertia-go/vite"
)

//go:embed app.html
var views embed.FS

//go:embed all:frontend/dist
var frontend embed.FS

func init() {
	var key [32]byte
	_, _ = rand.Read(key[:])
	store, _ := session.NewCookie(session.CookieOptions{Keys: [][]byte{key[:]}})

	i, err := inertia.New(inertia.Config{
		RootView:   "app.html",
		TemplateFS: views,
		Version:    "v1",
		Session:    store,
		Vite:       viteHelper(),
	})
	if err != nil {
		panic(err)
	}

	dist, err := fs.Sub(frontend, "frontend/dist")
	if err != nil {
		panic(err)
	}
	static := http.FileServerFS(dist)

	// The middleware is required: it parses the Inertia request headers and
	// drains the cookie session into the response.
	app := i.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i.Render(w, r, "Home", inertia.Props{
			"greeting": "Hello from Go!",
		})
	}))

	spinhttp.Handle(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			static.ServeHTTP(w, r)
			return
		}

		app.ServeHTTP(w, r)
	})
}

// viteHelper points at the Vite dev server when vite_dev_server is set, and
// otherwise reads the production manifest from the embedded build.
func viteHelper() inertia.ViteHelper {
	if dev := os.Getenv("vite_dev_server"); dev != "" {
		return vite.Dev(dev)
	}

	dist, err := fs.Sub(frontend, "frontend/dist")
	if err != nil {
		return nil
	}

	assets := loadViteAssets(dist)
	if assets == nil {
		return nil
	}
	return assets
}

// main function must be included for the compiler but is not executed.
func main() {}
