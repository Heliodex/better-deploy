//go:build wasip1

package main

import (
	"context"
	"net/http"

	"github.com/heliodex/better-deploy/wt/packages/app"
	"github.com/heliodex/better-deploy/wt/packages/storage"
	wasihttp "github.com/heliodex/better-deploy/wt/packages/wasihttp"
)

// init is the only wasm-specific wiring: storage, the app routes and the wasi:http export.
func init() {
	store, storeErr := storage.New(wasihttp.NewClient())

	var (
		handler http.Handler
		migrate func(context.Context) error
	)

	if storeErr == nil {
		application := newNotesApp(store)
		handler = application.handler()
		migrate = application.migrate
	}

	wasihttp.Handle(app.ServeStorage(storeErr, migrate, handler).ServeHTTP)
}
