//go:build wasip1

package main

import (
	"context"
	"net/http"

	"wt.packages/app"
	"wt.packages/storage"
	wasihttp "wt.packages/wasihttp"
)

// init is the only wasm-specific wiring: storage, the app routes and the wasi:http export.
func init() {
	store, storeErr := storage.New(wasihttp.NewClient())

	var handler http.Handler

	var migrate func(context.Context) error

	if storeErr == nil {
		application := newPassportApp(store, wasihttp.NewClient())
		handler = application.handler()
		migrate = application.migrate
	}

	wasihttp.Handle(app.ServeStorage(storeErr, migrate, handler).ServeHTTP)
}
