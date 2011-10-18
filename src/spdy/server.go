package spdy

import (
	"crypto/rand"
	"crypto/tls"
	"http"
	"net"
	"os"
	"time"
)

func serverConnectThread(sock net.Conn, handler http.Handler, fallback chan net.Conn) {
	// Hand the connection off to the standard HTTPS server
	if t, ok := sock.(*tls.Conn); ok {
		if t.ConnectionState().NegotiatedProtocol != "spdy/3" {
			if fallback != nil {
				fallback <- sock
			} else {
				sock.Close()
			}
			return
		}
	}

	c := newConnection(sock.RemoteAddr(), handler, true)
	c.run(sock)
}

// serve runs the server accept loop
func serve(listener net.Listener, handler http.Handler, fallback chan net.Conn) os.Error {

	if handler == nil {
		handler = http.DefaultServeMux
	}

	for {
		sock, err := listener.Accept()
		if err != nil {
			return err
		}

		// Do the TLS negotation on a seperate thread to avoid
		// blocking the accept loop
		go serverConnectThread(sock, handler, fallback)
	}

	panic("unreachable")
}

// ListenAndServe listens for unencrypted SPDY connections on addr.
//
// Because it does not use TLS/SSL of this it can't use the next protocol
// negotation in TLS to fall back on standard HTTP.
func ListenAndServe(addr string, handler http.Handler) os.Error {
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
//
// It uses the TLS next negotation protocol to fallback on standard https.
func ListenAndServeTLS(addr string, certFile string, keyFile string, handler http.Handler) os.Error {
	if addr == "" {
		addr = ":https"
	}
	cfg := &tls.Config{
		Rand:         rand.Reader,
		Time:         time.Seconds,
		NextProtos:   []string{"spdy/3", "http/1.1"},
		Certificates: make([]tls.Certificate, 1),
	}

	var err os.Error
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
		error:  make(chan os.Error),
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
	error  chan os.Error
	accept chan net.Conn
	addr   net.Addr
}

func (s *httpsListener) Accept() (net.Conn, os.Error) {
	select {
	case err := <-s.error:
		return nil, err
	case sock := <-s.accept:
		return sock, nil
	}
	panic("unreachable")
}

func (s *httpsListener) Close() os.Error {
	return nil
}

func (s *httpsListener) Addr() net.Addr {
	return s.addr
}
