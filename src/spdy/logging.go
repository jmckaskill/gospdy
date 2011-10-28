package spdy

import (
	"fmt"
	"log"
	"net"
	"os"
)

type connLogger struct {
	net.Conn
	prefix string
}

func NewConnLogger(prefix string, c net.Conn) net.Conn {
	return &connLogger{c, prefix}
}

func (l *connLogger) Close() (err os.Error) {
	err = l.Conn.Close()
	if err != nil {
		log.Printf("%s Close: %v", l.prefix, err)
	} else {
		log.Printf("%s Close", l.prefix)
	}
	return
}

func (l *connLogger) Read(p []byte) (n int, err os.Error) {
	n, err = l.Conn.Read(p)
	if err != nil {
		log.Printf("%s Read %s: %v", l.prefix, p[0:n], err)
	} else {
		log.Printf("%s Read %s", l.prefix, p[0:n])
	}
	return
}

func (l *connLogger) Write(p []byte) (n int, err os.Error) {
	n, err = l.Conn.Write(p)
	if err != nil {
		log.Printf("%s Write %s: %v", l.prefix, p[0:n], err)
	} else {
		log.Printf("%s Write %s", l.prefix, p[0:n])
	}
	return
}

type listenLogger struct {
	net.Listener
	prefix string
}

func NewListenLogger(prefix string, c net.Listener) net.Listener {
	return &listenLogger{c, prefix}
}

func (l *listenLogger) Accept() (c net.Conn, err os.Error) {
	c, err = l.Listener.Accept()
	if err != nil {
		log.Printf("%s Accept: %v", l.prefix, err)
	} else {
		log.Printf("%s Accept %s", l.prefix, c.RemoteAddr())
	}
	c = NewConnLogger(fmt.Sprintf("%s %s", l.prefix, c.RemoteAddr()), c)
	return
}
