package spdy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"http"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"strconv"
	"strings"
	"url"
)

var DefaultTransport http.RoundTripper = &Transport{Proxy: http.ProxyFromEnvironment}
var DefaultClient = &http.Client{Transport: DefaultTransport}

// requestTxThread pushes the request body down the stream
func requestTxThread(body io.ReadCloser, s *streamTxUser) {
	io.Copy(s, body)
	s.Close()
	body.Close()
}

// startRequest starts a new request and starts pushing the request body and
// waits for the reply (if non unidirectional).
func (c *Connection) startRequest(req *http.Request, parentStream *stream, childHandler http.Handler) (resp *http.Response, err os.Error) {
	priority := DefaultPriority
	pstr := req.Header.Get(":priority")
	if p, err := strconv.Atoi(pstr); len(pstr) > 0 && err != nil && 0 <= p && p < MaxPriority {
		priority = p
	}

	rxFinished := len(req.Header.Get(":unidirectional")) > 0
	txFinished := req.Body == nil

	body := req.Body
	req.Body = nil

	s := new(stream)
	s.connection = c
	s.cond = sync.NewCond(&s.lock)
	s.request = req

	s.rxFinished = rxFinished

	s.txClosed = txFinished
	s.txFinished = txFinished
	s.txPriority = priority
	s.shouldSendReply = false

	s.txWindow = defaultWindow

	s.closeChannel = make(chan bool)

	s.parent = parentStream

	s.handler = childHandler

	// Send the SYN_REQUEST
	select {
	case <-c.goAwayChannel:
		return nil, ErrGoAway
	case c.onStartRequest <- s:
	}

	// Start the request body push
	if !txFinished {
		go requestTxThread(body, (*streamTxUser)(s))
	}

	// Wait for the reply
	if rxFinished {
		return nil, nil
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	for s.response == nil && s.closeError == nil {
		s.cond.Wait()
	}

	return s.response, s.closeError
}

// RoundTrip starts a new request on the connection and then waits for the
// response header.
//
// To change the priority of the request set the ":priority" header field to a
// number between 0 (highest) and MaxPriority-1 (lowest). Otherwise
// DefaultPriority will be used.
//
// To start an unidirectional request where we do not wait for the response,
// set the ":unidirectional" header to a non empty value. The return value
// resp will then be nil.
//
// This can be safely called from any thread.
func (c *Connection) RoundTrip(req *http.Request) (resp *http.Response, err os.Error) {
	return c.startRequest(req, nil, nil)
}

type Transport struct {
	Proxy           func(*http.Request) (*url.URL, os.Error)
	Dial            func(net, addr string) (c net.Conn, err os.Error)
	TLSClientConfig *tls.Config
	FallbackClient  http.Client

	lk          sync.Mutex
	connections map[string]*Connection // key is proxy_url|host:port
}

// Given a string of the form "host", "host:port", or "[ipv6::address]:port",
// return true if the string includes a port.
func hasPort(s string) bool {
	return strings.LastIndex(s, ":") > strings.LastIndex(s, "]")
}

func addDefaultPort(s string, port int) string {
	if hasPort(s) {
		return s
	}
	return fmt.Sprintf("%s:%d", s, port)
}

func removePort(s string) string {
	if !hasPort(s) {
		return s
	}
	return s[:strings.LastIndex(s, ":")]
}

func connKey(proxy *url.URL, req *http.Request) string {
	proxyStr := ""
	if proxy != nil {
		proxyStr = proxy.String()
	}
	hostStr := addDefaultPort(req.URL.Host, 443)
	return strings.Join([]string{proxyStr, hostStr}, "|")
}

func (t *Transport) dialProxy(proxy *url.URL, addr string) (net.Conn, os.Error) {
	log.Printf("dialProxy %+v %s", proxy, addr)
	dial := t.Dial
	if dial == nil {
		dial = net.Dial
	}

	if proxy == nil {
		return dial("tcp", addDefaultPort(addr, 443))
	}

	if proxy.Scheme != "http" {
		return nil, ErrUnsupportedProxy
	}

	conn, err := dial("tcp", addDefaultPort(proxy.Host, 80))
	if err != nil {
		return nil, err
	}

	req := &http.Request{
		Method: "CONNECT",
		RawURL: addDefaultPort(addr, 443),
		Host:   addDefaultPort(addr, 443),
		Header: make(http.Header),
	}
	// TODO(james): Proxy-Authentication

	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}

	// Read response.
	// Okay to use and discard buffered reader here, because
	// TLS server will not speak until spoken to.
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != 200 {
		f := strings.SplitN(resp.Status, " ", 2)
		conn.Close()
		return nil, os.NewError(f[1])
	}

	return conn, nil
}

func (t *Transport) runClient(key string, c *Connection) {
	c.Run()
	t.lk.Lock()
	if t.connections[key] == c {
		t.connections[key] = nil, false
	}
	t.lk.Unlock()
}

func (t *Transport) RoundTrip(req *http.Request) (resp *http.Response, err os.Error) {
	if req.URL == nil {
		if req.URL, err = url.Parse(req.RawURL); err != nil {
			return
		}
	}

	if req.URL.Scheme != "https" {
		return t.FallbackClient.Do(req)
	}

	var proxy *url.URL
	if t.Proxy != nil {
		if proxy, err = t.Proxy(req); err != nil {
			return nil, err
		}
	}

	key := connKey(proxy, req)

reconnect:
	t.lk.Lock()
	c := t.connections[key]

	// Try and use an existing connection
	if c == nil {
		proxySock, err := t.dialProxy(proxy, req.URL.Host)
		if err != nil {
			t.lk.Unlock()
			return nil, err
		}

		cfg := tls.Config{}
		if t.TLSClientConfig != nil {
			cfg = *t.TLSClientConfig
		}

		cfg.NextProtos = []string{"http/1.1", "spdy/3", "spdy/2"}
		cfg.ServerName = removePort(req.URL.Host)

		tlsSock := tls.Client(proxySock, &cfg)
		if err := tlsSock.Handshake(); err != nil {
			t.lk.Unlock()
			tlsSock.Close()
			proxySock.Close()
			return nil, err
		}

		if err := tlsSock.VerifyHostname(cfg.ServerName); err != nil {
			t.lk.Unlock()
			tlsSock.Close()
			proxySock.Close()
			return nil, err
		}

		switch tlsSock.ConnectionState().NegotiatedProtocol {
		case "http/1.1":
			// fallback to a standard HTTPS client
			t.lk.Unlock()
			client := http.NewClientConn(tlsSock, nil)
			resp, err := client.Do(req)
			client.Close()
			tlsSock.Close()
			proxySock.Close()
			return resp, err

		case "spdy/2":
			c = NewConnection(tlsSock, nil, 2, false)
		case "spdy/3":
			c = NewConnection(tlsSock, nil, 3, false)
		default:
			panic("spdy-internal: unexpected negotiated protocol")
		}

		if t.connections == nil {
			t.connections = make(map[string]*Connection)
		}
		t.connections[key] = c
		go t.runClient(key, c)
	}

	t.lk.Unlock()
	resp, err = c.startRequest(req, nil, nil)

	// In the case that we missed the connection due to being told to go
	// away, we need to reconnect. This is due to either the server
	// sending us a GO_AWAY or the startRequest going through after the
	// socket has already been closed due to an error or timeout.
	if err == ErrGoAway {
		goto reconnect
	}

	return resp, err
}
