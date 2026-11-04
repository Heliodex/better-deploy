//go:build wasip1

package main

import (
	"context"
	"net/http"

	"github.com/heliodex/better-deploy/wt/packages/v2/app"
	"github.com/heliodex/better-deploy/wt/packages/v2/storage"
	wasihttp "github.com/heliodex/better-deploy/wt/packages/v2/wasihttp"
)

// init is the only wasm-specific wiring: storage, the app routes and the wasi:http export.
func init() {
	store, storeErr := storage.New(wasihttp.NewClient())

	var handler http.Handler

	var migrate func(context.Context) error

	if storeErr == nil {
		application, err := newPassportApp(store, wasihttp.NewClient())
		if err != nil {
			storeErr = err
		} else {
			handler = application.handler()
			migrate = application.migrate
		}
	}

	wasihttp.Handle(app.ServeStorage(storeErr, migrate, handler).ServeHTTP)
}
