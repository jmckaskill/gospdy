package spdy

import (
	"io"
	"os"
	"net"
	"http"
	"sync"
	"strconv"
)

// NewClient creates a SPDY client connection around sock.
//
// sock should be the underlying socket already connected. Typically this is a
// TLS connection which has already gone the next protocol negotiation, but
// any socket will work.
//
// Handler is used to provide the callback for any content pushed from the
// server. If it is nil then pushed streams are refused.
func NewClient(sock net.Conn, handler http.Handler) http.RoundTripper {
	c := newConnection(sock.RemoteAddr(), handler, false)
	go c.run(sock)
	return c
}

// requestTxThread pushes the request body down the stream
func requestTxThread(body io.ReadCloser, s *streamTx) {
	io.Copy(s, body)
	s.Close()
	body.Close()
}

// startRequest starts a new request and starts pushing the request body and
// waits for the reply (if non unidirectional).
func (c *connection) startRequest(req *http.Request, parentStream *stream, childHandler http.Handler) (resp *http.Response, err os.Error) {
	priority := DefaultPriority
	if p, err := strconv.Atoi(req.Header.Get(":priority")); err != nil && 0 <= p && p < MaxPriority {
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
	c.onStartRequest <- s

	// Start the request body push
	if !txFinished {
		go requestTxThread(body, (*streamTx)(s))
	}

	// Wait for the reply
	if rxFinished {
		return nil, nil
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	for s.response == nil && s.closeError != nil {
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
func (c *connection) RoundTrip(req *http.Request) (resp *http.Response, err os.Error) {
	return c.startRequest(req, nil, nil)
}
