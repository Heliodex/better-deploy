package main

import (
	"crypto/rand"
	"embed"
	"net/http"

	spinhttp "github.com/spinframework/spin-go-sdk/v3/http"

	"github.com/inertia-go/inertia-go"
	"github.com/inertia-go/inertia-go/session"
)

//go:embed app.html
var views embed.FS

func init() {
	var key [32]byte
	_, _ = rand.Read(key[:])
	store, _ := session.NewCookie(session.CookieOptions{Keys: [][]byte{key[:]}})

	i, _ := inertia.New(inertia.Config{
		RootView:   "app.html",
		TemplateFS: views,
		Version:    "v1",
		Session:    store,
	})

	// The middleware is required: it parses the Inertia request headers and drains the cookie session into the response.
	handler := i.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i.Render(w, r, "Home", inertia.Props{
			"greeting": "Hello from Go!",
		})
	}))

	spinhttp.Handle(handler.ServeHTTP)
}

// main function must be included for the compiler but is not executed.
func main() {}
