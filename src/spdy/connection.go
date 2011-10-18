package spdy

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"http"
	"io"
	"log"
	"net"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
)

// data in connections are only accessible on the connection dispatch thread
type connection struct {
	// general connection info
	handler    http.Handler
	remoteAddr net.Addr
	tls        *tls.ConnectionState
	rxWindow   int

	// tx thread channels
	sendControl chan frame
	sendData    [maxPriorities]chan frame

	// dispatch thread channels
	onStartRequest chan *stream // do not use directly, use startRequest instead
	onFinishedSent chan *stream // channel to notify the dispatch thread that FLAG_FIN has been sent

	// stream info
	streams          map[int]*stream
	lastStreamOpened int
	nextStreamId     int

	nextPingId uint32
}

// nextTxFrame gets the next frame to be written to the socket in prioritized
// order.
func nextTxFrame(control chan frame, data []chan frame) frame {
	// try a non-blocking receive in priority order
	select {
	case f := <-control:
		return f
	default:
	}

	for _, ch := range data {
		select {
		case f := <-ch:
			return f
		default:
		}
	}

	// do a blocking receive on all the send channels
	select {
	case f := <-control:
		return f
	case f := <-data[0]:
		return f
	case f := <-data[1]:
		return f
	case f := <-data[2]:
		return f
	case f := <-data[3]:
		return f
	case f := <-data[4]:
		return f
	case f := <-data[5]:
		return f
	case f := <-data[6]:
		return f
	case f := <-data[7]:
		return f
	}

	panic("unreachable")
}

// txPump runs the connection transmit loop which receives frames from the
// session tx threads and writes them out to the underlying socket. The frames
// are prioritized by receiving from a number of send channels which are
// polled from highest priority to lowest before blocking on them all.
func txPump(sock io.WriteCloser, control chan frame, data []chan frame) {
	zip := compressor{}

	for {
		f := nextTxFrame(control, data)

		if err := f.WriteTo(sock, &zip); err != nil {
			break
		}
	}

	sock.Close()
}

// rxPump runs the connection receive loop for both client and server
// connections. It then finds the message boundaries and sends each one over
// to the connection thread.
func rxPump(sock io.ReadCloser, dispatch chan []byte, dispatched chan os.Error) {

	buf := new(buffer)

	for {
		d, err := buf.Get(sock, 8)
		if err != nil {
			goto end
		}

		length := int(fromBig32(d[4:])&0xFFFFFF) + 8

		d, err = buf.Get(sock, length)
		// If we get an error due to the buffer overflowing, then
		// len(d) < 8 + length. We try and continue anyways, and the
		// disptach thread can decide whether we need to throw a
		// session error and disconnect or just send a stream error.
		if err != nil {
			goto end
		}

		dispatch <- d
		err = <-dispatched

		if err != nil {
			goto end
		}

		buf.Flush(len(d))
		length -= len(d)

		// If we couldn't buffer all of the message above, consume the
		// rest of the data in the message and dump the data on the
		// floor.
		for length > 0 {
			d, err := buf.Get(sock, length)
			if err != nil {
				goto end
			}

			buf.Flush(len(d))
			length -= len(d)
		}
	}

end:
	sock.Close()
}

// run runs the main connection thread which is responsible for dispatching
// messages to the streams and managing the list of streams.
//
// TLS handshaking is done here before launching the tx and rx pump threads so
// that the server accept loop is not held up.
func (c *connection) run(sock net.Conn) {
	var err os.Error
	unzip := decompressor{}

	if t, ok := sock.(*tls.Conn); ok {
		if err := t.Handshake(); err != nil {
			return
		}

		c.tls = new(tls.ConnectionState)
		*c.tls = t.ConnectionState()
	}

	dispatch := make(chan []byte)
	dispatched := make(chan os.Error)

	go txPump(sock, c.sendControl, c.sendData[:])
	go rxPump(sock, dispatch, dispatched)

	for {
		select {
		case s := <-c.onStartRequest:
			c.handleStartRequest(s)

		case s := <-c.onFinishedSent:
			c.checkStreamFinished(s)

		case d := <-dispatch:
			err = c.handleFrame(d, &unzip)
			dispatched <- err

			if err != nil {
				goto end
			}
		}
	}

end:
	streams := c.streams
	c.streams = nil
	for _, s := range streams {
		c.onStreamFinished(s, err)
	}

	sock.Close()
}

// onStreamFinished removes a completed stream.
//
// err is nil for successfull completion and non nil for aborts.
//
// It then shuts down the stream setting closeError so the stream rx/tx
// threads can see the error.
//
// It also removes the stream from the stream list so any further frames
// concerning this stream force a rstInvalidStream.
//
// Finally it recursively shuts down associated streams.
func (c *connection) onStreamFinished(s *stream, err os.Error) {
	if c.streams != nil {
		c.streams[s.streamId] = nil, false
	}

	// Disconnect child streams
	for _, a := range s.children {
		c.onStreamFinished(a, ErrAssociatedStreamClosed)
	}

	s.lock.Lock()
	s.rxFinished = true
	s.txFinished = true
	s.closeError = err
	close(s.closeChannel)
	s.cond.Broadcast()
	s.lock.Unlock()

	// Remove ourself from our parent
	if s.parent != nil && err != ErrAssociatedStreamClosed {
		a := s.parent
		for i, s2 := range a.children {
			if s2 == s {
				a.children = append(a.children[:i], a.children[i+1:]...)
				break
			}
		}
	}
}

// checkStreamFinished checks to see if we have successfully completed a
// stream and shuts it down if so.
func (c *connection) checkStreamFinished(s *stream) {
	// Also check c.streams to handle the case where the stream was
	// aborted right after it finished. Strictly speaking we shouldn't
	// read rxFinished without a lock and txFinished at all as they are
	// owned on different threads, but they are never unset once set so
	// this is fine.
	if s.txFinished && s.rxFinished && c.streams[s.streamId] == s {
		c.onStreamFinished(s, nil)
	}
}

func (c *connection) handleStartRequest(s *stream) {
	s.streamId = c.nextStreamId
	c.nextStreamId += 2

	assocId := 0
	if s.parent != nil {
		assocId = s.parent.streamId
	}

	if uint(s.streamId) > maxStreamId {
		// we haven't added the stream to
		// s.parent.children so reset the parent.
		s.parent = nil
		c.onStreamFinished(s, ErrTooManyStreams)
		return
	}

	// unidirectional and immediate finish messages never
	// get added to the streams table and will shortly be gc'd
	if !s.txFinished || !s.rxFinished {
		c.streams[s.streamId] = s

		if s.parent != nil {
			s.parent.children = append(s.parent.children, s)
		}
	}

	// note we always use the control channel to ensure that the
	// SYN_STREAM packets are sent out in the order in which the stream
	// ids were allocated
	c.sendControl <- &synStreamFrame{
		StreamId:           s.streamId,
		AssociatedStreamId: assocId,
		Finished:           s.txFinished,
		Unidirectional:     s.rxFinished,
		Header:             s.request.Header,
		Priority:           s.txPriority,
		URL:                s.request.URL,
		Proto:              s.request.Proto,
		Method:             s.request.Method,
	}
}

func handlerThread(h http.Handler, s *streamTx, req *http.Request) {
	defer func() {
		s.Close()
		err := recover()
		if err == nil {
			return
		}

		var buf bytes.Buffer
		fmt.Fprintf(&buf, "spdy: panic serving %d: %v\n", s.streamId, err)
		buf.Write(debug.Stack())
		log.Print(buf.String())
	}()

	h.ServeHTTP(s, req)
}

func (c *connection) handleSynStream(d []byte, unzip *decompressor) os.Error {
	f, err := parseSynStream(d, unzip)
	if err != nil {
		return err
	}

	// The remote has reopened an already opened stream. We kill both.
	if s2 := c.streams[f.StreamId]; s2 != nil {
		c.sendControl <- rstStreamFrame{f.StreamId, rstProtocolError}
		c.onStreamFinished(s2, ErrStreamInUse)
		return nil
	}

	// The remote tried to open a stream of the wrong type (eg its a
	// client and tried to open a server stream).
	if (f.StreamId & 1) == (c.nextStreamId & 1) {
		c.sendControl <- rstStreamFrame{f.StreamId, rstProtocolError}
		return nil
	}

	// Stream Ids must monotonically increase
	if f.StreamId <= c.lastStreamOpened {
		c.sendControl <- rstStreamFrame{f.StreamId, rstProtocolError}
		return nil
	}
	c.lastStreamOpened = f.StreamId

	// The handler is either the connection global one or the associated
	// stream one.
	handler := c.handler
	var parent *stream

	if f.AssociatedStreamId > 0 {
		// You are only allowed to open associated streams to streams
		// that you are the recipient.
		if (f.AssociatedStreamId & 1) != (c.nextStreamId & 1) {
			c.sendControl <- rstStreamFrame{f.StreamId, rstProtocolError}
		}
		parent = c.streams[f.AssociatedStreamId]
		// The remote tried to open a stream associated with a closed
		// stream. We kill this new stream.
		if parent != nil {
			c.sendControl <- rstStreamFrame{f.StreamId, rstInvalidStream}
			return nil
		}

		handler = parent.handler
	}

	if handler == nil {
		c.sendControl <- rstStreamFrame{f.StreamId, rstRefusedStream}
		return nil
	}

	// The SYN_STREAM passed all of our tests, so go ahead and create the
	// stream, hook it up and start a request handler thread.

	s := new(stream)
	s.streamId = f.StreamId
	s.connection = c

	s.cond = sync.NewCond(&s.lock)

	s.rxFinished = f.Finished

	s.txClosed = f.Unidirectional
	s.txFinished = f.Unidirectional
	s.txPriority = f.Priority
	s.shouldSendReply = !f.Unidirectional

	s.txWindow = defaultWindow

	s.closeChannel = make(chan bool)

	// Messages that have both their rx and tx pipes already closed don't
	// need to be added to the streams table.
	if !s.txFinished || !s.rxFinished {
		c.streams[f.StreamId] = s

		if parent != nil {
			parent.children = append(parent.children, s)
			s.parent = parent
		}
	}

	r := &http.Request{
		Method:           f.Method,
		RawURL:           f.URL.String(),
		URL:              f.URL,
		Proto:            f.Proto,
		Header:           f.Header,
		Body:             (*streamRx)(s),
		TransferEncoding: []string{},
		Close:            true,
		Host:             f.URL.Host,
		RemoteAddr:       c.remoteAddr.String(),
		TLS:              c.tls,
	}

	if cl, err := strconv.Atoi64(f.Header.Get("Content-Length")); err != nil {
		r.ContentLength = cl
	}

	if slash := strings.LastIndex(f.Proto, "/"); slash >= 0 {
		fmt.Sscanf(f.Proto[slash+1:], "%d.%d", &r.ProtoMajor, &r.ProtoMinor)
	}

	go handlerThread(handler, (*streamTx)(s), r)

	return nil
}

func (c *connection) handleSynReply(d []byte, unzip *decompressor) os.Error {
	f, err := parseSynReply(d, unzip)
	if err != nil {
		return err
	}

	s := c.streams[f.StreamId]
	if s == nil {
		c.sendControl <- rstStreamFrame{f.StreamId, rstInvalidStream}
		return nil
	}

	if s.response != nil {
		c.sendControl <- rstStreamFrame{f.StreamId, rstStreamInUse}
		c.onStreamFinished(s, ErrStreamInUse)
		return nil
	}

	if s.rxFinished {
		c.sendControl <- rstStreamFrame{f.StreamId, rstStreamAlreadyClosed}
		c.onStreamFinished(s, ErrStreamAlreadyClosed)
		return nil
	}

	r := &http.Response{
		Status:           f.Status,
		Proto:            f.Proto,
		Header:           f.Header,
		Body:             (*streamRx)(s),
		TransferEncoding: []string{},
		Close:            true,
		Request:          s.request,
	}

	if code, err := strconv.Atoi(f.Status); err != nil {
		r.StatusCode = code
	}

	if cl, err := strconv.Atoi64(f.Header.Get("Content-Length")); err != nil {
		r.ContentLength = cl
	}

	if slash := strings.LastIndex(f.Proto, "/"); slash >= 0 {
		fmt.Sscanf(f.Proto[slash+1:], "%d.%d", &r.ProtoMajor, &r.ProtoMinor)
	}

	s.lock.Lock()
	s.response = r
	s.cond.Broadcast()
	s.lock.Unlock()

	if f.Finished {
		s.rxFinished = true
		c.checkStreamFinished(s)
	}

	return nil
}

func (c *connection) handleHeaders(d []byte, unzip *decompressor) os.Error {
	f, err := parseHeaders(d, unzip)
	if err != nil {
		return err
	}

	s := c.streams[f.StreamId]
	if s == nil {
		c.sendControl <- rstStreamFrame{f.StreamId, rstInvalidStream}
		return nil
	}

	if !f.Finished {
		return nil
	}

	s.rxFinished = true
	c.checkStreamFinished(s)

	return nil
}

func (c *connection) handleRstStream(d []byte) os.Error {
	f, err := parseRstStream(d)
	if err != nil {
		return err
	}

	s := c.streams[f.StreamId]
	if s == nil {
		// ignore resets for closed streams
		return nil
	}

	err = ErrProtocolError
	switch f.Reason {
	case rstInvalidStream:
		err = ErrInvalidStream
	case rstRefusedStream:
		err = ErrRefusedStream
	case rstUnsupportedVersion:
		err = ErrUnsupportedVersion
	case rstCancel:
		err = ErrCancel
	case rstFlowControlError:
		err = ErrFlowControl
	case rstStreamInUse:
		err = ErrStreamInUse
	case rstStreamAlreadyClosed:
		err = ErrStreamAlreadyClosed
	}

	c.onStreamFinished(s, err)
	return nil
}

func (c *connection) handleSettings(d []byte) os.Error {
	f, err := parseSettings(d)
	if err != nil {
		return err
	}

	if !f.HaveWindow {
		return nil
	}

	change := f.Window - c.rxWindow
	c.rxWindow = f.Window

	for _, s := range c.streams {
		s.lock.Lock()
		s.txWindow += change
		s.cond.Broadcast()
		s.lock.Unlock()
	}

	return nil
}

func (c *connection) handleWindowUpdate(d []byte) os.Error {
	f, err := parseWindowUpdate(d)
	if err != nil {
		return err
	}

	s := c.streams[f.StreamId]
	if s == nil {
		c.sendControl <- rstStreamFrame{f.StreamId, rstInvalidStream}
		return nil
	}

	s.lock.Lock()
	s.txWindow += f.WindowDelta
	s.cond.Broadcast()
	s.lock.Unlock()

	return nil
}

func (c *connection) handlePing(d []byte) os.Error {
	f, err := parsePing(d)
	if err != nil {
		return err
	}

	// To ignore loopback pings we need to check the bottom bit.
	if (f.Id & 1) != (c.nextPingId & 1) {
		c.sendControl <- pingFrame{f.Id}
	}

	return nil
}

func (c *connection) handleData(d []byte) os.Error {
	f, err := parseData(d)
	if err != nil {
		return err
	}

	s := c.streams[f.StreamId]
	if s == nil {
		c.sendControl <- rstStreamFrame{f.StreamId, rstInvalidStream}
		return nil
	}

	if s.rxFinished {
		c.sendControl <- rstStreamFrame{f.StreamId, rstStreamAlreadyClosed}
		c.onStreamFinished(s, ErrStreamAlreadyClosed)
		return nil
	}

	// The rx pump thread could not give us the entire message due to it
	// being too large.
	if length := int(fromBig32(d[4:]) & 0xFFFFFF); length != len(f.Data) {
		c.sendControl <- rstStreamFrame{f.StreamId, rstFlowControlError}
		c.onStreamFinished(s, ErrFlowControl)
		return nil
	}

	s.lock.Lock()

	if s.rxBuffer == nil {
		s.rxBuffer = bytes.NewBuffer(make([]byte, 0, len(f.Data)))
	}

	s.rxBuffer.Write(f.Data)
	s.cond.Broadcast()
	s.lock.Unlock()

	if f.Finished {
		s.rxFinished = true
		c.checkStreamFinished(s)
	} else {
		c.sendControl <- windowUpdateFrame{s.streamId, len(f.Data)}
	}

	return nil
}

func (c *connection) handleFrame(d []byte, unzip *decompressor) os.Error {
	code := fromBig32(d[0:])

	if code&0x80000000 == 0 {
		return c.handleData(d)
	}

	if length := int(fromBig32(d[4:]) & 0xFFFFFF); length != len(d) {
		return parseError
	}

	switch code {
	case synStreamCode:
		return c.handleSynStream(d, unzip)

	case synReplyCode:
		return c.handleSynReply(d, unzip)

	case rstStreamCode:
		return c.handleRstStream(d)

	case settingsCode:
		return c.handleSettings(d)

	case pingCode:
		return c.handlePing(d)

	case windowUpdateCode:
		return c.handleWindowUpdate(d)

	case headersCode:
		return c.handleHeaders(d, unzip)
	}

	ver := (code >> 16) & 0x7FFF
	ctype := code & 0xFFFF

	// Reply with unsupported version for SYN_STREAM. This should be
	// sufficient as its the first message sent to open a stream. We can't
	// cover the version in the general case, because we don't know what
	// stream id to abort.
	if ver != Version && ctype == synStreamControlType && len(d) > 12 {
		streamId := int(fromBig32(d[8:]))
		c.sendControl <- rstStreamFrame{streamId, rstUnsupportedVersion}
	}

	// Messages with the correct version but unknown type are ignored.

	return nil
}

// newConnection allocates a new connection
func newConnection(remoteAddr net.Addr, handler http.Handler, server bool) *connection {
	c := &connection{
		handler:          handler,
		remoteAddr:       remoteAddr,
		rxWindow:         defaultWindow,
		sendControl:      make(chan frame),
		onStartRequest:   make(chan *stream),
		onFinishedSent:   make(chan *stream),
		streams:          make(map[int]*stream),
		lastStreamOpened: 0,
	}

	for i := 0; i < len(c.sendData); i++ {
		c.sendData[i] = make(chan frame)
	}

	if server {
		c.nextStreamId = 2
		c.nextPingId = 0
	} else {
		c.nextStreamId = 1
		c.nextPingId = 1
	}

	return c
}
