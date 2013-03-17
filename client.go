package spdy

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
)

var DefaultTransport http.RoundTripper = &Transport{
	Proxy:          http.ProxyFromEnvironment,
	FallbackClient: http.DefaultClient,
}

var DefaultClient = &http.Client{Transport: DefaultTransport}

// sendRequestBody pushes the request body down the stream
func sendRequestBody(body io.ReadCloser, s *streamTx) {
	io.Copy(s, body)
	s.close()
	body.Close()
}

type emptyReply int

func (emptyReply) Read([]byte) (int, error) { return 0, io.EOF }
func (emptyReply) Close() error             { return nil }

// startRequest starts a new request and starts pushing the request body and
// waits for the reply (if non unidirectional).
func (c *Connection) startRequest(parent *stream, req *http.Request, extra *RequestExtra) (resp *http.Response, err error) {
	if extra == nil {
		extra = &RequestExtra{}
	}

	txFinished := req.Body == nil
	body := req.Body
	req.Body = nil

	s := c.newStream(req, txFinished, extra)
	s.parent = parent

	// Send the SYN_REQUEST
	select {
	case <-c.onGoAway:
		return nil, errGoAway
	case c.onStartRequest <- s:
	}

	if err := <-c.onRequestStarted; err != nil {
		return nil, err
	}

	if !txFinished {
		go sendRequestBody(body, (*streamTx)(s))
	}

	// For unidirectional requests, we return a fake response as
	// http.Client expects something
	if extra.Unidirectional {
		r := &http.Response{
			Status:        "200 OK",
			StatusCode:    200,
			Proto:         "HTTP/1.0",
			ProtoMajor:    1,
			ProtoMinor:    0,
			Body:          emptyReply(0),
			ContentLength: -1,
			Request:       req,
		}
		return r, nil
	}

	// Wait for the reply
	s.rxLock.Lock()
	defer s.rxLock.Unlock()

	for s.rxResponse == nil && s.rxError == nil {
		s.rxCond.Wait()
	}

	return s.rxResponse, s.rxError
}

type Transport struct {
	RequestExtra

	Proxy           func(*http.Request) (*url.URL, error)
	Dial            func(net, addr string) (c net.Conn, err error)
	TLSClientConfig *tls.Config
	FallbackClient  *http.Client

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

func (t *Transport) dialProxy(proxy *url.URL, addr string) (net.Conn, error) {
	dial := t.Dial
	if dial == nil {
		dial = net.Dial
	}
	addr = addDefaultPort(addr, 443)

	if proxy == nil {
		return dial("tcp", addr)
	}

	if proxy.Scheme != "http" {
		return nil, errUnsupportedProxy(proxy.Scheme)
	}

	conn, err := dial("tcp", addDefaultPort(proxy.Host, 80))
	if err != nil {
		return nil, err
	}

	req := &http.Request{
		Method: "CONNECT",
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
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
		return nil, errors.New(f[1])
	}

	return conn, nil
}

func (t *Transport) runClient(key string, c *Connection) {
	c.Run()
	t.lk.Lock()
	if t.connections[key] == c {
		delete(t.connections, key)
	}
	t.lk.Unlock()
}

func (t *Transport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	if req.URL == nil {
		return nil, errors.New("http: nil Request.URL")
	}
	if req.Header == nil {
		return nil, errors.New("http: nil Request.Header")
	}

	if req.URL.Scheme != "https" {
		if t.FallbackClient == nil {
			return nil, errors.New(fmt.Sprintf("spdy: no fallback client for scheme %s", req.URL.Scheme))
		}

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

		if !cfg.InsecureSkipVerify {
			if err := tlsSock.VerifyHostname(cfg.ServerName); err != nil {
				t.lk.Unlock()
				tlsSock.Close()
				proxySock.Close()
				return nil, err
			}
		}

		Log("negotiated protocol %s", tlsSock.ConnectionState().NegotiatedProtocol)

		switch tlsSock.ConnectionState().NegotiatedProtocol {
		case "http/1.1":
			// fallback to a standard HTTPS client
			t.lk.Unlock()
			client := httputil.NewClientConn(tlsSock, nil)
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
	resp, err = c.startRequest(nil, req, &t.RequestExtra)

	// In the case that we missed the connection due to being told to go
	// away, we need to reconnect. This is due to either the server
	// sending us a GO_AWAY or the startRequest going through after the
	// socket has already been closed due to an error or timeout.
	if err == errGoAway {
		goto reconnect
	}

	return resp, err
}
