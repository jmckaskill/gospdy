package spdy

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"fmt"
	"http"
	"io"
	"os"
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
	rxLock       sync.Mutex
	rxCond       *sync.Cond
	rxResponse   *http.Response
	rxBuffer     bytes.Buffer
	rxCompressed bool // whether the data is transparently compressed or not
	rxFinished   bool
	rxError      os.Error

	// Receive data only used by the dispatch thread
	rxHaveData bool

	// Receive thread data, only accessed by the rx thread
	rxClosed bool
	rxReader io.Reader

	// Transmit data, shared between dispatch and tx thread
	txLock   sync.Mutex
	txCond   *sync.Cond
	txWindow int
	txError  os.Error

	// channel that is closed when rxError and txError is set to wake up
	// the rx and tx thread if it is blocked on sending to the connection
	// send thread
	errorChannel chan bool

	// Transmit data, only accessed by the tx thread
	txClosed           bool // streamTxUser.Close has been called
	txPriority         int
	txCompressed       bool
	txBuffered         bool
	txFinished         bool
	txWriter           flushWriteCloser
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
	Flush() os.Error
	io.WriteCloser
}

type RequestExtra struct {
	Unidirectional    bool
	Priority          int
	Buffered          bool
	Compressed        bool
	AssociatedHandler http.Handler
}

type Stream interface {
	http.ResponseWriter
	http.Flusher
	SetPriority(priority int)
	Priority() int
	EnableOutputCompression(compressed bool)
	EnableOutputBuffering(buffering bool)
	PushRequest(req *http.Request, extra *RequestExtra) (*http.Response, os.Error)
	RoundTrip(r *http.Request) (*http.Response, os.Error)
}

// Split the user accessible stream methods into seperate sets

// Used as the txWriter output
type streamTxOut stream

// Given to the user when they can write as the ResponseWriter. The user can
// then cast to a spdy.Stream to push associated requests, set the priority,
// enable compression/buffering, etc.
type streamTxUser stream

var _ Stream = (*streamTxUser)(nil)

// Given to the user when the can read
type streamRxUser stream

// Seperate type so we can do transparent decompression
type streamRxIn stream

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
	s.txBuffered = extra.Compressed

	s.childHandler = extra.AssociatedHandler
	return s
}

func (s *streamRxIn) Read(buf []byte) (int, os.Error) {
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
			// Ignore the error this time around, it will be
			// picked up on the next read
		}
	}

	return n, err
}

// Read reads request/response data.
//
// This is called by the resp.Body.Read by the user after starting a request.
//
// It is also called by the user to get request data in request.Body.Read.
//
// This will return os.EOF when all data has been successfully read without
// getting a SPDY RST_STREAM (equivalent of an abort).
func (s *streamRxUser) Read(buf []byte) (n int, err os.Error) {
	if s.rxReader == nil {
		// Do a zero length read so we can wait for some data to
		// arrive, so we can tell if its compressed or not.
		if _, err := (*streamRxIn)(s).Read([]byte{}); err != nil {
			return 0, err
		}

		// Once we receive data, rxCompressed is not changed so its
		// safe to read this without relocking rxLock.
		if s.rxCompressed {
			s.rxReader, err = zlib.NewReader((*streamRxIn)(s))
			if err != nil {
				return 0, err
			}
		} else {
			s.rxReader = (*streamRxIn)(s)
		}
	}

	return s.rxReader.Read(buf)
}

// Closes the rx channel
func (s *streamRxUser) Close() os.Error {
	// We don't care about recipients closing the request rx early
	if s.isRecipient || s.rxClosed {
		return nil
	}

	s.rxLock.Lock()
	defer s.rxLock.Unlock()

	s.rxClosed = true
	s.connection.onStreamFinished <- (*stream)(s)
	return s.rxError
}

// PushRequest starts a new pushed request associated with this request.
func (s *streamTxUser) PushRequest(req *http.Request, extra *RequestExtra) (resp *http.Response, err os.Error) {
	return s.connection.startRequest((*stream)(s), req, extra)
}

func (s *streamTxUser) RoundTrip(req *http.Request) (*http.Response, os.Error) {
	return s.PushRequest(req, nil)
}

// Header returns the response header so that headers can be changed.
//
// The header should not be altered after WriteHeader or Write has been
// called.
func (s *streamTxUser) Header() http.Header {
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
func (s *streamTxUser) WriteHeader(status int) {
	if s.replyHeaderWritten {
		panic("spdy: writing header twice")
	}
	s.replyHeaderWritten = true
	s.replyStatus = status
	if !s.txBuffered {
		// TODO(james): what to do with an error?
		_ = (*stream)(s).sendReplyIfNeeded(false)
	}
}

func (s *streamTxUser) Priority() int {
	return s.txPriority
}

func (s *streamTxUser) SetPriority(priority int) {
	s.txPriority = priority
}

func (s *streamTxUser) EnableOutputCompression(compressed bool) {
	if s.txClosed {
		return
	}

	// compression is not supported in V2
	if s.connection.version < 3 {
		return
	}

	if s.replyHeaderWritten || s.txWriter != nil {
		panic("spdy: changing output after Write has been called")
	}

	s.txCompressed = compressed
}

func (s *streamTxUser) EnableOutputBuffering(buffered bool) {
	if s.txClosed {
		return
	}

	if s.replyHeaderWritten || s.txWriter != nil {
		panic("spdy: changing output after Write has been called")
	}

	s.txBuffered = buffered
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
func (s *streamTxUser) Write(data []byte) (n int, err os.Error) {
	if s.txClosed {
		return 0, ErrWriteAfterClose
	}

	if len(data) == 0 {
		return 0, nil
	}

	if !s.replyHeaderWritten {
		s.WriteHeader(http.StatusOK)
	}

	if s.txWriter == nil {
		out := (*streamTxOut)(s)
		if s.txCompressed {
			buf := bufio.NewWriter(out)
			zip, err := zlib.NewWriter(buf)
			if err != nil {
				return 0, err
			}

			s.txWriter = &flushingCompressor{zip, buf, s.txBuffered}

		} else if s.txBuffered {
			s.txWriter = closeableWriteBuffer{bufio.NewWriter(out)}
		} else {
			s.txWriter = out
		}
	}

	return s.txWriter.Write(data)
}

type flushingCompressor struct {
	zip      *zlib.Writer
	buf      *bufio.Writer
	buffered bool
}

func (s *flushingCompressor) Write(p []byte) (int, os.Error) {
	n, err := s.zip.Write(p)
	if err != nil {
		return n, err
	}
	if !s.buffered {
		return n, s.Flush()
	}
	return n, err
}

func (s *flushingCompressor) Flush() os.Error {
	s.zip.Flush()
	return s.buf.Flush()
}

func (s *flushingCompressor) Close() os.Error {
	s.zip.Close()
	return s.buf.Flush()
}

type closeableWriteBuffer struct {
	*bufio.Writer
}

func (s closeableWriteBuffer) Close() os.Error {
	return s.Writer.Flush()
}

// CloseTx closes the tx pipe and flushes any buffered data. This is
// called after the handler callback has finished and after pushing through
// the request body.
func (s *stream) closeTx() {
	if s.txClosed {
		// This can happen if the tx is preclosed (eg a unidirectional
		// reply)
		return
	}

	s.txClosed = true
	replySendFinished := s.txWriter == nil

	if err := s.sendReplyIfNeeded(replySendFinished); err != nil {
		// This can happen if the remote kills the stream before we
		// send the reply, in which case we end up sending nothing
		return
	}

	if s.txWriter != nil {
		if err := s.txWriter.Close(); err != nil {
			return
		}
	}

	// In most cases the close will have already been sent with the last
	// of the data as it got flushed through, but in cases where no data
	// was buffered (eg if it already been flushed, or buffuring was
	// disabled) then we send an empty data frame with the finished flag
	// here.
	if s.txFinished {
		return
	}

	f := &dataFrame{
		Finished:   true,
		Compressed: s.txCompressed,
		StreamId:   s.streamId,
	}

	s.sendFrame(f)
	s.txFinished = true
}

// Flush flushes data being written to the sessions tx thread which flushes it
// out the socket.
func (s *streamTxUser) Flush() {
	if s.txClosed {
		return
	}

	if err := (*stream)(s).sendReplyIfNeeded(false); err != nil {
		return
	}

	if s.txWriter != nil {
		if err := s.txWriter.Flush(); err != nil {
			return
		}
	}
}

// sendFrame sends a frame to the session tx thread, which sends it out the
// socket.
func (s *stream) sendFrame(f frame) os.Error {
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
func (s *stream) sendReplyIfNeeded(finished bool) os.Error {
	if s.replySent || !s.isRecipient {
		return nil
	}

	if !s.replyHeaderWritten {
		(*streamTxUser)(s).WriteHeader(http.StatusOK)
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
func (s *streamTxOut) amountOfDataToSend(want int) (int, os.Error) {
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

// Noops so we can disable buffering
func (s *streamTxOut) Flush() os.Error {
	return nil
}

func (s *streamTxOut) Close() os.Error {
	return nil
}

// Function hooked up to the output of s.txWriter to flush data to the session
// tx thread.
func (s *streamTxOut) Write(data []byte) (int, os.Error) {
	// If this is the first call and is due to the tx buffer filling up,
	// then the reply hasn't yet been sent.
	if err := (*stream)(s).sendReplyIfNeeded(false); err != nil {
		return 0, err
	}

	sent := 0
	for sent < len(data) {
		var err os.Error
		tosend := len(data) - sent

		tosend, err = s.amountOfDataToSend(tosend)
		if err != nil {
			return sent, err
		}

		f := &dataFrame{
			Finished:   s.txClosed && sent+tosend == len(data),
			Compressed: s.txCompressed,
			Data:       data[sent : sent+tosend],
			StreamId:   s.streamId,
		}

		s.txFinished = f.Finished
		if err := (*stream)(s).sendFrame(f); err != nil {
			return sent, err
		}

		sent += tosend
	}

	return sent, nil
}
