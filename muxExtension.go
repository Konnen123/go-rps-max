package main

import (
	"net/http"
)

type appMux struct {
	mux *http.ServeMux
}

func (aMux appMux) HandleHttpFunc(requestMethod string, pattern string, handler http.HandlerFunc) {
	aMux.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", requestMethod)
		if r.Method != requestMethod {
			return
		}
		handler(w, r)
	})
}
