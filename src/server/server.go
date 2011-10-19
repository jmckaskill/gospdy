package main

import (
	"http"
	"log"
	"spdy"
)

type redirect int

func (s redirect) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "https://www.foobar.co.nz"+r.URL.RawPath, http.StatusMovedPermanently)
}

func main() {
	go http.ListenAndServe(":80", redirect(0))

	if err := spdy.ListenAndServeTLS(":443", "foobar.co.nz.crt", "foobar.co.nz.key", nil); err != nil {
		log.Print(err)
	}
}
