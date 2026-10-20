package main

import (
	"crypto/rand"
	"net/http"
	"os"

	spinhttp "github.com/spinframework/spin-go-sdk/v3/http"

	"github.com/inertia-go/inertia-go"
	"github.com/inertia-go/inertia-go/session"
)

func init() {
	var key [32]byte
	_, _ = rand.Read(key[:])
	store, _ := session.NewCookie(session.CookieOptions{Keys: [][]byte{key[:]}})

	i, _ := inertia.New(inertia.Config{
		RootView:   "app.html",
		TemplateFS: os.DirFS("views"),
		Version:    "v1",
		Session:    store,
	})

	spinhttp.Handle(func(w http.ResponseWriter, r *http.Request) {
		i.Render(w, r, "Home", inertia.Props{
			"greeting": "Hello from Go!",
		})
	})
}

// main function must be included for the compiler but is not executed.
func main() {}
