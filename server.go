package spdy

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"time"
)

func serverConnectThread(sock net.Conn, handler http.Handler, fallback chan net.Conn) {
	addr := sock.RemoteAddr()

	version := 2

	if t, ok := sock.(*tls.Conn); ok {
		if err := t.Handshake(); err != nil {
			return
		}

		s := t.ConnectionState()

		if !s.NegotiatedProtocolIsMutual ||
			s.NegotiatedProtocol == "" ||
			s.NegotiatedProtocol == "http/1.1" {

			// Hand the connection off to the standard HTTPS server
			if fallback != nil {
				fallback <- sock
			} else {
				sock.Close()
			}
			return
		}

		switch t.ConnectionState().NegotiatedProtocol {
		case "spdy/2":
			version = 2
		case "spdy/3":
			version = 3
		default:
			panic("spdy-internal: unexpected negotiated protocol")
		}
	}

	defer func() {
		if err := recover(); err != nil {
			var buf bytes.Buffer
			fmt.Fprintf(&buf, "spdy: panic serving %s: %v\n", addr.String(), err)
			buf.Write(debug.Stack())
			log.Print(buf.String())
		}
	}()

	c := NewConnection(sock, handler, version, true)
	c.Run()
}

// serve runs the server accept loop
func serve(listener net.Listener, handler http.Handler, fallback chan net.Conn) error {

	if handler == nil {
		handler = http.DefaultServeMux
	}

	for {
		sock, err := listener.Accept()
		if err != nil {
			return err
		}

		Log("accept %s\n", sock.RemoteAddr())

		// Do the TLS negotation on a seperate thread to avoid
		// blocking the accept loop
		go serverConnectThread(sock, handler, fallback)
	}

	panic("unreachable")
}

// ListenAndServe listens for unencrypted SPDY connections on addr. Because it
// does not use TLS/SSL of this it can't use the next protocol negotation in
// TLS to fall back on standard HTTP.
func ListenAndServe(addr string, handler http.Handler) error {
	if addr == "" {
		addr = ":http"
	}
	conn, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return serve(conn, handler, nil)
}

// ListenAndServeTLS listens for encrpyted SPDY or HTTPS connections on addr.
// It uses the TLS next negotation protocol to fallback on standard https.
func ListenAndServeTLS(addr string, certFile string, keyFile string, handler http.Handler) error {
	if addr == "" {
		addr = ":https"
	}
	cfg := &tls.Config{
		Rand:         rand.Reader,
		Time:         time.Now,
		NextProtos:   []string{"spdy/3", "spdy/2", "http/1.1"},
		Certificates: make([]tls.Certificate, 1),
	}

	var err error
	cfg.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}

	conn, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	tlsListener := tls.NewListener(conn, cfg)

	fallback := &httpsListener{
		error:  make(chan error),
		accept: make(chan net.Conn),
		addr:   tlsListener.Addr(),
	}

	go (&http.Server{Addr: addr, Handler: handler}).Serve(fallback)

	err = serve(tlsListener, handler, fallback.accept)
	fallback.error <- err
	return err
}

// httpsListener is a fake listener for feeding to the standard HTTPS server.
//
// This is so that we can hand it connections which negotiate https as their
// protocol through TLS next protocol negotation.
type httpsListener struct {
	error  chan error
	accept chan net.Conn
	addr   net.Addr
}

func (s *httpsListener) Accept() (net.Conn, error) {
	select {
	case err := <-s.error:
		return nil, err
	case sock := <-s.accept:
		return sock, nil
	}
	panic("unreachable")
}

func (s *httpsListener) Close() error {
	return nil
}

func (s *httpsListener) Addr() net.Addr {
	return s.addr
}
