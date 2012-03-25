package spdy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sync"
)

const (
	maxPriorities     = 8
	defaultBufferSize = 64 * 1024
	defaultWindow     = 64 * 1024
	maxStreamId       = 0x7FFFFFFF
	maxDataPacketSize = 4 * 1024
)

type stream struct {
	// Data setup when the stream is first created/registered and not
	// changed thereafter
	streamId    int
	connection  *Connection
	request     *http.Request
	isRecipient bool

	// Receive data, shared between dispatch and rx thread, data must be
	// accessed with a lock and the condition variable is used to signal
	// updates
	rxLock     sync.Mutex
	rxCond     *sync.Cond
	rxResponse *http.Response
	rxBuffer   bytes.Buffer
	rxFinished bool
	rxError    error

	// Receive thread data, only accessed by the rx thread
	rxClosed bool

	// Transmit data, shared between dispatch and tx thread, accessed with
	// the lock and the cond variable is used to signal updates.
	txLock   sync.Mutex
	txCond   *sync.Cond
	txWindow int
	txError  error

	// channel that is closed when rxError and txError is set to wake up
	// the rx and tx thread if it is blocked on sending to the connection
	// send thread
	errorChannel chan bool

	// Transmit data, only accessed by the tx thread
	txClosed           bool // streamTxUser.Close has been called
	txPriority         int
	txFinished         bool
	replySent          bool
	replyHeaderWritten bool
	replyHeader        http.Header
	replyStatus        int

	// Data used by the dispatch thread for handling associated streams. A
	// stream's associated stream (as specified in that streams
	// SYN_STREAM) is its parent.
	children     []*stream
	parent       *stream
	childHandler http.Handler
}

type flushWriteCloser interface {
	Flush() error
	io.WriteCloser
}

type RequestExtra struct {
	Unidirectional    bool
	Priority          int
	AssociatedHandler http.Handler
}

type Stream interface {
	http.ResponseWriter
	http.Flusher
	SetPriority(priority int)
	Priority() int
	PushRequest(req *http.Request, extra *RequestExtra) (*http.Response, error)
	RoundTrip(r *http.Request) (*http.Response, error)
}

// Split the user accessible stream methods into seperate sets

// Given to the user when they can write as the ResponseWriter. The user can
// then cast to a spdy.Stream to push associated requests, set the priority,
// enable compression/buffering, etc.
type streamTx stream

var _ Stream = (*streamTx)(nil)

// Given to the user which they can read
type streamRx stream

func (c *Connection) newStream(req *http.Request, txFinished bool, extra *RequestExtra) *stream {
	s := new(stream)
	s.connection = c
	s.request = req
	s.isRecipient = false

	s.rxCond = sync.NewCond(&s.rxLock)
	s.rxFinished = extra.Unidirectional
	s.rxClosed = extra.Unidirectional

	s.txCond = sync.NewCond(&s.txLock)
	s.txWindow = defaultWindow

	s.errorChannel = make(chan bool)

	s.txClosed = txFinished
	s.txFinished = txFinished
	s.txPriority = extra.Priority

	s.childHandler = extra.AssociatedHandler
	return s
}

// Read reads request/response data.
//
// This is called by the resp.Body.Read by the user after starting a request.
//
// It is also called by the user to get request data in request.Body.Read.
//
// This will return os.EOF when all data has been successfully read without
// getting a SPDY RST_STREAM (equivalent of an abort).
func (s *streamRx) Read(buf []byte) (int, error) {
	if s.rxClosed {
		return 0, errReadAfterClose
	}

	s.rxLock.Lock()

	for !s.rxFinished && s.rxBuffer.Len() == 0 && s.rxError == nil {
		s.rxCond.Wait()
	}

	if s.rxError != nil {
		s.rxLock.Unlock()
		return 0, s.rxError
	}

	rxFinished := s.rxFinished
	n, err := s.rxBuffer.Read(buf)
	s.rxLock.Unlock()

	c := s.connection

	// TODO(james) reduce how often we are sending window updates
	if !rxFinished && c.version >= 3 && n > 0 {
		f := &windowUpdateFrame{
			Version:     c.version,
			StreamId:    s.streamId,
			WindowDelta: n,
		}

		select {
		case c.sendWindowUpdate <- f:
		case <-s.errorChannel:
			// TODO(james): this error used to be ignored, is this
			// safe?
			err = s.rxError
		}
	}

	return n, err
}

// Closes the rx channel
func (s *streamRx) Close() error {
	// We don't care about recipients closing the request rx early
	if s.isRecipient || s.rxClosed {
		return nil
	}

	s.rxClosed = true

	s.rxLock.Lock()
	defer s.rxLock.Unlock()
	s.connection.onStreamFinished <- (*stream)(s)
	return s.rxError
}

// PushRequest starts a new pushed request associated with this request.
func (s *streamTx) PushRequest(req *http.Request, extra *RequestExtra) (resp *http.Response, err error) {
	return s.connection.startRequest((*stream)(s), req, extra)
}

func (s *streamTx) RoundTrip(req *http.Request) (*http.Response, error) {
	return s.PushRequest(req, nil)
}

// Header returns the response header so that headers can be changed.
//
// The header should not be altered after WriteHeader or Write has been
// called.
func (s *streamTx) Header() http.Header {
	if s.replyHeader == nil {
		s.replyHeader = make(http.Header)
	}
	return s.replyHeader
}

// WriteHeader writes the response header.
//
// The header will be buffered until the next Flush, the handler function
// returns or when the tx buffer fills up.
//
// The Header() should not be changed after calling this.
func (s *streamTx) WriteHeader(status int) {
	if s.replyHeaderWritten {
		panic("spdy: writing header twice")
	}
	s.replyHeaderWritten = true
	s.replyStatus = status
}

func (s *streamTx) Priority() int {
	return s.txPriority
}

func (s *streamTx) SetPriority(priority int) {
	s.txPriority = priority
}

// Flush sends the headers and SYN_REPLY if they haven't already been sent. As
// there isn't any data buffering, this is otherwise a noop.
func (s *streamTx) Flush() {
	s.Write(nil)
}

// Write writes response body data.
//
// This will call WriteHeader if it hasn't been already called.
//
// The data will be buffered and then actually sent the next time Flush is
// called, when the handler function returns, or when the tx buffer fills up.
//
// This function is also used by the request tx pump to send request body
// data.
func (s *streamTx) Write(data []byte) (n int, err error) {
	if s.txClosed {
		return 0, errWriteAfterClose
	}

	if !s.replyHeaderWritten {
		s.WriteHeader(http.StatusOK)
	}

	if err := s.sendReplyIfNeeded(false); err != nil {
		return 0, err
	}

	// Write(nil) and Flush() do the same thing as we don't have any
	// buffering, which is to send the headers and SYN_REPLY.
	if len(data) == 0 {
		return 0, nil
	}

	sent := 0
	for sent < len(data) {
		var err error
		tosend := len(data) - sent

		tosend, err = s.amountOfDataToSend(tosend)
		if err != nil {
			return sent, err
		}

		f := &dataFrame{
			Finished: s.txClosed && sent+tosend == len(data),
			Data:     data[sent : sent+tosend],
			StreamId: s.streamId,
		}

		s.txFinished = f.Finished
		if err := s.sendFrame(f); err != nil {
			return sent, err
		}

		sent += tosend
	}

	return sent, nil
}

// Close closes the tx pipe and flushes any buffered data. This is called
// after the handler callback has finished and after pushing through the
// request body.
func (s *streamTx) close() {
	if s.txClosed {
		// This can happen if the tx is preclosed (eg a unidirectional
		// reply)
		return
	}

	s.txClosed = true

	if err := s.sendReplyIfNeeded(true); err != nil {
		// This can happen if the remote kills the stream before we
		// send the reply, in which case we end up sending nothing
		return
	}

	// In most cases the close will have already been sent with the last
	// of the data as it got flushed through, but in cases where no data
	// was buffered (eg if it already been flushed) then we send an empty
	// data frame with the finished flag here.
	if s.txFinished {
		return
	}

	f := &dataFrame{
		Finished: true,
		StreamId: s.streamId,
	}

	s.sendFrame(f)
	s.txFinished = true
}

// sendFrame sends a frame to the session tx thread, which sends it out the
// socket.
func (s *streamTx) sendFrame(f frame) error {
	c := s.connection

	pri := s.txPriority - HighPriority
	if pri < 0 {
		pri = 0
	} else if pri >= len(c.sendData) {
		pri = len(c.sendData) - 1
	}

	select {
	case <-s.errorChannel:
		return s.txError
	case c.sendData[pri] <- f:
	}

	return <-c.dataSent
}

// sendReply sends the SYN_REPLY frame which contains the response headers.
// Note this won't be called until the first flush or the tx channel is closed.
func (s *streamTx) sendReplyIfNeeded(finished bool) error {
	if s.replySent || !s.isRecipient {
		return nil
	}

	if !s.replyHeaderWritten {
		s.WriteHeader(http.StatusOK)
	}

	f := &synReplyFrame{
		Version:  s.connection.version,
		Finished: finished,
		StreamId: s.streamId,
		Header:   s.replyHeader,
		Status:   fmt.Sprintf("%d %s", s.replyStatus, http.StatusText(s.replyStatus)),
		Proto:    "HTTP/1.1",
	}

	s.replySent = true
	s.txFinished = finished
	return s.sendFrame(f)
}

// amountOfDataToSend figures out how much data we can send, potentially
// waiting for a WINDOW_UPDATE frame from the remote. It only returns once we
// can send > 0 bytes or the remote sent a RST_STREAM to abort.
func (s *streamTx) amountOfDataToSend(want int) (int, error) {
	if want > maxDataPacketSize {
		want = maxDataPacketSize
	}

	if s.connection.version < 3 {
		return want, nil
	}

	s.txLock.Lock()
	defer s.txLock.Unlock()

	for s.txWindow <= 0 && s.txError == nil {
		s.txCond.Wait()
	}

	if s.txError != nil {
		return 0, s.txError
	}

	if want > s.txWindow {
		want = s.txWindow
	}

	s.txWindow -= want
	return want, nil
}
