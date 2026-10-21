package main

import (
	"crypto/sha256"
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"slices"
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
	store, err := session.NewCookie(session.CookieOptions{Keys: [][]byte{sessionKey()}})
	if err != nil {
		panic(err)
	}

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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", renderHome(i))
	mux.HandleFunc("POST /messages", createMessage(i))

	// The middleware is required: it parses the Inertia request headers and
	// drains the cookie session into the response.
	app := i.Middleware(mux)

	spinhttp.Handle(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			static.ServeHTTP(w, r)
			return
		}

		app.ServeHTTP(w, r)
	})
}

// renderHome renders the guestbook with its messages as Inertia props.
func renderHome(i *inertia.Inertia) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		messages, err := loadMessages()
		if err != nil {
			slog.ErrorContext(r.Context(), "guestbook: load messages", "err", err)
			messages = []Message{}
		}
		slices.Reverse(messages)

		i.Render(w, r, "Home", inertia.Props{
			"greeting": "Hello from Go!",
			"messages": messages,
		})
	}
}

// createMessage validates the submitted form, stores the message, and
// redirects back to the guestbook. Validation errors and the flash message
// travel through the Inertia session and arrive as page props.
func createMessage(i *inertia.Inertia) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)

		payload, err := readMessagePayload(r)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}

		name := strings.TrimSpace(payload.Name)
		body := strings.TrimSpace(payload.Body)

		if errs := validateMessage(name, body); len(errs) > 0 {
			collector := inertia.ValidationErrors(r)
			for field, message := range errs {
				collector.Add(field, message)
			}
			i.Back(w, r)
			return
		}

		if err := addMessage(name, body); err != nil {
			slog.ErrorContext(r.Context(), "guestbook: add message", "err", err)
			inertia.Flash(r).Set("error", "Your message could not be saved. Please try again.")
			i.Back(w, r)
			return
		}

		inertia.Flash(r).Set("success", "Your message was posted.")
		i.Redirect(w, r, "/")
	}
}

// sessionKey derives a stable encryption key for the flash cookie. Spin may
// run each request in a fresh instance, so the key cannot be generated
// randomly at startup. Set session_key to the same value on every replica in
// production; the fallback is for local development only.
func sessionKey() []byte {
	secret := os.Getenv("session_key")
	if secret == "" {
		secret = "dev-only-insecure-session-key"
	}

	sum := sha256.Sum256([]byte(secret))
	return sum[:]
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
