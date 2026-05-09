package main

import (
	"net/http"
)

type appMux struct {
	mux *http.ServeMux
}

func (aMux appMux) HandleHttpFunc(requestMethod string, pattern string, handler http.HandlerFunc) {
	aMux.mux.HandleFunc(requestMethod+" "+pattern, handler)
}
