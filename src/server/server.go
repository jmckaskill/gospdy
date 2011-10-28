package main

import (
	"bytes"
	"encoding/base64"
	"http"
	"io"
	"log"
	"os"
	"spdy"
	"strings"
	"sync"
	"url"
	"websocket"
)

func unauth(w http.ResponseWriter) bool {
	w.Header().Add("WWW-Authenticate", "Basic realm=\"trimbletools.com\"")
	w.WriteHeader(http.StatusUnauthorized)
	return false
}

func basicAuth(w http.ResponseWriter, r *http.Request, auth func(user, pass string) bool) bool {
	a := r.Header.Get("Authorization")
	if a == "" {
		return unauth(w)
	}

	if !strings.HasPrefix(a, "Basic ") {
		return unauth(w)
	}

	d, err := base64.StdEncoding.DecodeString(a[len("Basic "):])
	if err != nil {
		return unauth(w)
	}

	colon := bytes.IndexByte(d, ':')
	if colon < 0 {
		return unauth(w)
	}

	user := string(d[:colon])
	pass := string(d[colon+1:])

	if !auth(user, pass) {
		return unauth(w)
	}

	return true
}

func removeAuth(r *http.Request) {
	r.Header.Del("Authorization")
}

func auth(user, pass string) bool {
	return user == "james" && pass == "trimble"
}

type nullWriter int

func (s nullWriter) Write(b []byte) (n int, err os.Error) {
	return len(b), nil
}

// Removes the root path element from the URL returning it.
func splitPathRoot(url *url.URL) (dir string) {
	// strip the leading slash
	path := url.Path[1:]

	// find the next slash
	slash := strings.Index(path, "/") + 1

	if slash > 0 {
		dir = url.Path[1:slash]
		url.Path = url.Path[slash:]
		url.RawPath = url.RawPath[slash:]
	} else {
		dir = url.Path[1:]
		url.Path = "/"
		url.RawPath = url.RawPath[len(url.Path):]
	}

	return
}

var proxyLock sync.Mutex
var proxyStream spdy.Stream

func bind(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		return
	}

	ps, ok := w.(spdy.Stream)

	proxyLock.Lock()
	if !ok || proxyStream != nil {
		proxyLock.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	proxyStream = ps
	proxyLock.Unlock()

	ps.WriteHeader(http.StatusOK)
	ps.Flush()

	// Keep on reading until the request times out, cancels, etc
	if _, err := io.Copy(nullWriter(0), r.Body); err != nil {
		log.Print(err)
	}

	proxyLock.Lock()
	if proxyStream == ps {
		proxyStream = nil
	}
	proxyLock.Unlock()
}

type base64Encoder struct {
	io.Writer
}

func (s base64Encoder) Write(data []byte) (int, os.Error) {
	enc := make([]byte, base64.StdEncoding.EncodedLen(len(data)))
	base64.StdEncoding.Encode(enc, data)
	_, err := s.Writer.Write(enc)
	return len(data), err
}

func vnc(ws *websocket.Conn) {
	defer log.Print("vnc finish")
	proxyLock.Lock()
	ps := proxyStream
	proxyLock.Unlock()

	if ps == nil {
		return
	}

	// Use our own encoder that flushes on each Write
	w := base64Encoder{ws}
	r := base64.NewDecoder(base64.StdEncoding, ws)

	req, err := http.NewRequest("GET", "https://www.foobar.co.nz/vnc", r)
	if err != nil {
		log.Print(err)
		return
	}

	extra := &spdy.RequestExtra{Compressed: true}
	resp, err := ps.PushRequest(req, extra)
	if err != nil {
		log.Print(err)
		return
	}

	_, err = io.Copy(w, resp.Body)
	resp.Body.Close()
}

type redirect int

func (s redirect) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	http.Redirect(w, r, "https://www.foobar.co.nz"+r.URL.RawPath, http.StatusMovedPermanently)
}

type handler int

func (s handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/proxy/") && !basicAuth(w, r, auth) {
		return
	}

	removeAuth(r)

	log.Printf("Request %+v", r)
	http.DefaultServeMux.ServeHTTP(w, r)
}

func main() {
	log.SetFlags(log.Lmicroseconds)
	//sock, _ := net.Listen("tcp", ":80")
	http.HandleFunc("/bind", bind)
	http.Handle("/proxy/vnc", websocket.Handler(vnc))
	http.Handle("/vnc/", http.FileServer(http.Dir(".")))

	//go http.Serve(spdy.NewListenLogger("http", sock), handler(0))

	if err := spdy.ListenAndServeTLS(":1443", "trimbletools.crt", "trimbletools.key", handler(0)); err != nil {
		log.Print(err)
	}
}
