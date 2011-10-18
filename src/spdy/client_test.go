package spdy

import (
	"crypto/tls"
	"http"
	"log"
	"net"
	"os"
	"testing"
)

type connLogger struct {
	net.Conn
	prefix string
}

func NewConnLogger(prefix string, c net.Conn) net.Conn {
	return &connLogger{c, prefix}
}

func (s *connLogger) Read(b []byte) (n int, err os.Error) {
	log.Printf("%s read", s.prefix)
	n, err = s.Conn.Read(b)
	if err != nil {
		log.Printf("%s read %x: %v", s.prefix, b[0:n], err)
	} else {
		log.Printf("%s read %x", s.prefix, b[0:n])
	}
	return
}

func (s *connLogger) Write(b []byte) (n int, err os.Error) {
	log.Printf("%s write", s.prefix)
	n, err = s.Conn.Write(b)
	if err != nil {
		log.Printf("%s write %x: %v", s.prefix, b[0:n], err)
	} else {
		log.Printf("%s write %x", s.prefix, b[0:n])
	}
	return
}

func (s *connLogger) Close() os.Error {
	log.Printf("%s close", s.prefix)
	return s.Conn.Close()
}

func TestNewClient(t *testing.T) {
	cfg := tls.Config{
		NextProtos: []string{"spdy/3"},
		ServerName: "www.google.com",
	}

	sock, err := tls.Dial("tcp", "www.google.com:443", &cfg)
	if err != nil {
		t.Error(err)
		return
	}

	c := &http.Client{
		Transport: NewClient(NewConnLogger("client", sock), nil),
	}
	log.Print(c.Transport)

	req, err := http.NewRequest("GET", "https://www.google.com/index.html", nil)
	log.Printf("%+v %v %v", req, err, req.URL.String())
	resp, err := c.Get("https://www.google.com/index.html")
	if err != nil {
		t.Error(err)
		return
	}

	t.Log(resp)
	sock.Close()
}
