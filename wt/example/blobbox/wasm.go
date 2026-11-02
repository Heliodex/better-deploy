//go:build wasip1

package main

import (
	"context"
	"net/http"

	"wt.packages/app"
	"wt.packages/storage"
	wasihttp "wt.packages/wasihttp"
)

// clientForBlob is the wasi:http client used for the storage-loopback blob delete.
func clientForBlob() *http.Client {
	return wasihttp.NewClient()
}

func init() {
	store, storeErr := storage.New(wasihttp.NewClient())
	var handler http.Handler
	var migrate func(context.Context) error
	if storeErr == nil {
		application := newBlobApp(store, clientForBlob())
		handler = application.handler()
		migrate = application.migrate
	}
	wasihttp.Handle(app.ServeStorage(storeErr, migrate, handler).ServeHTTP)
}
