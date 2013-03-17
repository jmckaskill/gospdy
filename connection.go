package spdy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
)

var Log = func(fmt string, args ...interface{}) {}

// data in connections are only accessible on the connection dispatch thread
type Connection struct {
	// general connection info
	socket     net.Conn
	version    int
	handler    http.Handler
	remoteAddr net.Addr
	tls        *tls.ConnectionState
	rxWindow   int

	// tx thread channels
	sendControl      chan frame
	sendWindowUpdate chan frame
	sendData         [maxPriorities]chan frame
	dataSent         chan error

	// dispatch thread channels
	onStartRequest   chan *stream // do not use directly, use startRequest instead
	onRequestStarted chan error

	// For requests this happens when response.Body.Close is called. For
	// replies this happens when the handler function returns.
	onStreamFinished chan *stream

	// stream info
	streams          map[int]*stream
	lastStreamOpened int
	nextStreamId     int

	goAway   bool
	onGoAway chan bool

	nextPingId uint32
}

// nextTxFrame gets the next frame to be written to the socket in prioritized
// order. If it has to block it will flush the output buffer first.
func (c *Connection) nextTxFrame(buf *bufio.Writer) (frame, chan error) {
	// try a non-blocking receive in priority order

	select {
	case f := <-c.sendControl:
		return f, nil
	case f := <-c.sendWindowUpdate:
		return f, nil
	default:
	}

	for _, ch := range c.sendData {
		select {
		case f := <-ch:
			return f, c.dataSent
		default:
		}
	}

	buf.Flush()

	// do a blocking receive on all the send channels
	select {
	case f := <-c.sendControl:
		return f, nil
	case f := <-c.sendWindowUpdate:
		return f, nil
	case f := <-c.sendData[0]:
		return f, c.dataSent
	case f := <-c.sendData[1]:
		return f, c.dataSent
	case f := <-c.sendData[2]:
		return f, c.dataSent
	case f := <-c.sendData[3]:
		return f, c.dataSent
	case f := <-c.sendData[4]:
		return f, c.dataSent
	case f := <-c.sendData[5]:
		return f, c.dataSent
	case f := <-c.sendData[6]:
		return f, c.dataSent
	case f := <-c.sendData[7]:
		return f, c.dataSent
	}

	panic("unreachable")
}

// txPump runs the connection transmit loop which receives frames from the
// session tx threads and writes them out to the underlying socket. The frames
// are prioritized by receiving from a number of send channels which are
// polled from highest priority to lowest before blocking on them all.
func (c *Connection) txPump() {
	buf := bufio.NewWriter(c.socket)
	zip := compressor{}

	for {
		f, finish := c.nextTxFrame(buf)
		if f == nil {
			break
		}

		err := f.WriteFrame(c.socket, &zip)
		if finish != nil {
			finish <- err
		}
	}
}

// rxPump runs the connection receive loop for both client and server
// connections. It finds the message boundaries and sends each one over to the
// connection thread.
func (c *Connection) rxPump(dispatch chan []byte, dispatched chan error, rxError chan error) {

	buf := new(buffer)

	for {
		d, err := buf.Get(c.socket, 8)
		if err != nil {
			rxError <- err
			return
		}

		length := int(fromBig32(d[4:])&0xFFFFFF) + 8

		d, err = buf.Get(c.socket, length)
		// If the buffer overflows we will not get an error instead
		// len(d) < 8 + length. We try and continue anyways, and the
		// disptach thread can decide whether we need to throw a
		// session error and disconnect or just send a stream error.
		if err != nil {
			rxError <- err
			return
		}

		dispatch <- d
		err = <-dispatched

		if err != nil {
			rxError <- err
			return
		}

		buf.Flush(len(d))
		length -= len(d)

		// If we couldn't buffer all of the message above, consume the
		// rest of the data in the message and dump the data on the
		// floor.
		for length > 0 {
			d, err := buf.Get(c.socket, length)
			if err != nil {
				rxError <- err
				return
			}

			buf.Flush(len(d))
			length -= len(d)
		}
	}
}

// run runs the main connection thread which is responsible for dispatching
// messages to the streams and managing the list of streams.
func (c *Connection) Run() {
	unzip := decompressor{}

	if t, ok := c.socket.(*tls.Conn); ok {
		if err := t.Handshake(); err != nil {
			return
		}

		c.tls = new(tls.ConnectionState)
		*c.tls = t.ConnectionState()
	}

	dispatch := make(chan []byte)
	dispatched := make(chan error)
	rxError := make(chan error)

	go c.txPump()
	go c.rxPump(dispatch, dispatched, rxError)

	for {
		select {
		case s := <-c.onStartRequest:
			err := c.handleStartRequest(s)
			c.onRequestStarted <- err

		case s := <-c.onStreamFinished:
			// Handle the race where we sent/received a reset
			// before we handled this message.
			if c.streams[s.streamId] != s {
				break
			}

			if !s.isRecipient && !s.rxFinished {
				c.sendReset(s.streamId, rstCancel)
			}

			c.finishStream(s, errCancel(s.streamId))

		case d := <-dispatch:
			err := c.handleFrame(d, &unzip)

			if err == nil {
				dispatched <- nil
				break
			}

			serr, ok := err.(streamError)

			// Session error, we are going to abort the
			// connection. Send the error to the rx thread, it
			// will then send it back in rxError.
			if !ok {
				dispatched <- err
				break
			}

			// Stream error, abort the stream
			sid := serr.StreamId()
			c.sendReset(sid, serr.resetCode())

			if s := c.streams[sid]; s != nil {
				c.finishStream(s, err)
			}

			dispatched <- nil

		case err := <-rxError:
			// Session error, have to abort the whole connection
			c.goAway = true
			close(c.onGoAway)
			for _, s := range c.streams {
				c.finishStream(s, err)
			}

			// close the control channel to ensure that the tx
			// thread shuts down
			close(c.sendControl)
			c.socket.Close()
			return
		}
	}
}

/* finishStream removes a completed stream.
 *
 * It then shuts down the stream setting txError and rxError so the stream
 * rx/tx threads can see the error (if they are still running).
 *
 * It also removes the stream from the stream list so any further frames
 * concerning this stream force a rstInvalidStream.
 *
 * Finally it recursively shuts down associated streams.
 *
 * Close conditions:
 * 1. Error sent
 * 2. Error received
 *
 * 3. If requestor, when we close the response Body (rx closed). If the
 * request hasn't finished then we send a cancel RST_STREAM.
 *
 * 4. If recipient, when we finish the response. This is on the completion of
 * the handler callback. The request may not have been completely read. In
 * this case we do not send a RST_STREAM as the request may have been serviced
 * without reading the full request (eg if we errored with a HTTP error status
 * code - in this case the stream succeeded).
 */

func (c *Connection) finishStream(s *stream, err error) {
	// We use rxError != nil, etc to figure out if we have finished
	if err == nil {
		panic("")
	}

	delete(c.streams, s.streamId)

	// Disconnect child streams
	for _, a := range s.children {
		// Reset the parent pointer so the child doesn't try and
		// remove itself from the parent
		a.parent = nil
		c.finishStream(a, err)
	}

	s.rxLock.Lock()
	s.rxError = err
	s.rxCond.Broadcast()
	s.rxLock.Unlock()

	s.txLock.Lock()
	s.txError = err
	s.txCond.Broadcast()
	s.txLock.Unlock()

	close(s.errorChannel)

	// Remove ourself from our parent
	if s.parent != nil {
		p := s.parent
		for i, s2 := range p.children {
			if s2 == s {
				p.children = append(p.children[:i], p.children[i+1:]...)
				break
			}
		}
	}

	if c.goAway && len(c.streams) == 0 {
		c.socket.Close()
	}
}

func (c *Connection) sendReset(streamId int, reason int) {
	c.sendControl <- &rstStreamFrame{
		Version:  c.version,
		StreamId: streamId,
		Reason:   reason,
	}
}

/* handleStartRequest sends the SYN_STREAM and registers streams where we are
* the initiator */
func (c *Connection) handleStartRequest(s *stream) error {
	s.streamId = c.nextStreamId
	c.nextStreamId += 2

	assocId := 0
	if s.parent != nil {
		assocId = s.parent.streamId
	}

	if uint(s.streamId) > maxStreamId || c.goAway {
		return errGoAway
	}

	u := s.request.URL

	// Fixup the url if we need to set the scheme and prefer the host in
	// the request
	if u.Scheme == "" || u.Host != s.request.Host {
		u = new(url.URL)
		*u = *s.request.URL
		u.Host = s.request.Host

		if u.Scheme == "" {
			u.Scheme = "https"
		}
	}

	// note we always use the control channel to ensure that the
	// SYN_STREAM packets are sent out in the order in which the stream
	// ids were allocated
	c.sendControl <- &synStreamFrame{
		Version:            c.version,
		StreamId:           s.streamId,
		AssociatedStreamId: assocId,
		Finished:           s.txFinished,
		Unidirectional:     s.rxFinished,
		Header:             s.request.Header,
		Priority:           s.txPriority,
		URL:                u,
		Proto:              s.request.Proto,
		Method:             s.request.Method,
	}

	// unidirectional and immediate finish messages never
	// get added to the streams table and will shortly be gc'd
	if s.txFinished && s.rxFinished {
		return nil
	}

	c.streams[s.streamId] = s
	if s.parent != nil {
		s.parent.children = append(s.parent.children, s)
	}

	return nil
}

func handlerFinish(s *streamTx) {
	if err := recover(); err != nil {
		var buf bytes.Buffer
		fmt.Fprintf(&buf, "spdy: panic serving %d: %v\n", s.streamId, err)
		buf.Write(debug.Stack())
		log.Print(buf.String())
	}

	s.close()
	s.connection.onStreamFinished <- (*stream)(s)
}

func handlerThread(h http.Handler, s *streamTx, req *http.Request) {
	defer handlerFinish(s)
	h.ServeHTTP(s, req)
}

func (c *Connection) handleSynStream(d []byte, unzip *decompressor) error {
	f, err := parseSynStream(d, unzip)
	if err != nil {
		return err
	}

	Log("rx SYN_STREAM %+v\n", f)

	// The remote has reopened an already opened stream. We kill both.
	// Check this first as if any other check fails and this would've also
	// failed sending out the reset will invalidate the existing stream.
	if s2 := c.streams[f.StreamId]; s2 != nil {
		return errStreamInUse(f.StreamId)
	}

	if f.Version != c.version {
		return errStreamVersion{f.StreamId, f.Version}
	}

	// The remote tried to open a stream of the wrong type (eg its a
	// client and tried to open a server stream).
	if (f.StreamId & 1) == (c.nextStreamId & 1) {
		return errStreamProtocol(f.StreamId)
	}

	// Stream Ids must monotonically increase
	if f.StreamId <= c.lastStreamOpened {
		return errStreamProtocol(f.StreamId)
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
			return errStreamProtocol(f.StreamId)
		}
		parent = c.streams[f.AssociatedStreamId]
		// The remote tried to open a stream associated with a closed
		// stream. We kill this new stream.
		if parent == nil {
			return errInvalidAssociatedStream{f.StreamId, f.AssociatedStreamId}
		}

		handler = parent.childHandler
	}

	if handler == nil {
		return errRefusedStream(f.StreamId)
	}

	// The SYN_STREAM passed all of our tests, so go ahead and create the
	// stream, hook it up and start a request handler thread.

	r := &http.Request{
		Method:     f.Method,
		URL:        f.URL,
		Proto:      f.Proto,
		ProtoMajor: f.ProtoMajor,
		ProtoMinor: f.ProtoMinor,
		Header:     f.Header,
		Host:       f.URL.Host,
		RemoteAddr: c.remoteAddr.String(),
		RequestURI: f.URL.Path,
		TLS:        c.tls,
	}

	if cl, err := strconv.ParseInt(f.Header.Get("Content-Length"), 10, 64); err != nil {
		r.ContentLength = cl
	}

	extra := &RequestExtra{
		Unidirectional:    f.Finished,
		Priority:          f.Priority,
		AssociatedHandler: nil,
	}

	s := c.newStream(r, f.Unidirectional, extra)
	s.streamId = f.StreamId
	s.isRecipient = true
	s.request.Body = (*streamRx)(s)

	// Messages that have both their rx and tx pipes already closed don't
	// need to be added to the streams table.
	if !(s.txFinished && s.rxFinished) {
		c.streams[f.StreamId] = s

		if parent != nil {
			parent.children = append(parent.children, s)
			s.parent = parent
		}
	}

	go handlerThread(handler, (*streamTx)(s), r)
	return nil
}

func (c *Connection) handleSynReply(d []byte, unzip *decompressor) error {
	f, err := parseSynReply(d, unzip)
	if err != nil {
		return err
	}

	Log("rx SYN_REPLY %+v\n", f)

	s := c.streams[f.StreamId]
	if s == nil {
		return errInvalidStream(f.StreamId)
	}

	if f.Version != c.version {
		return errStreamVersion{f.StreamId, f.Version}
	}

	if s.rxResponse != nil {
		return errStreamInUse(f.StreamId)
	}

	if s.rxFinished {
		return errStreamAlreadyClosed(f.StreamId)
	}

	r := &http.Response{
		Status:     f.Status,
		Proto:      f.Proto,
		ProtoMajor: f.ProtoMajor,
		ProtoMinor: f.ProtoMinor,
		Header:     f.Header,
		Body:       (*streamRx)(s),
		Request:    s.request,
	}

	split := strings.SplitN(f.Status, " ", 2)

	if r.StatusCode, err = strconv.Atoi(split[0]); err != nil {
		return errStreamProtocol(f.StreamId)
	}

	if cl, err := strconv.ParseInt(f.Header.Get("Content-Length"), 10, 64); err == nil {
		r.ContentLength = cl
	}

	s.rxLock.Lock()
	s.rxResponse = r
	s.rxFinished = f.Finished
	s.rxCond.Broadcast()
	s.rxLock.Unlock()

	return nil
}

func (c *Connection) handleHeaders(d []byte, unzip *decompressor) error {
	f, err := parseHeaders(d, unzip)
	if err != nil {
		return err
	}

	Log("rx HEADERS %+v\n", f)

	s := c.streams[f.StreamId]
	if s == nil {
		return errInvalidStream(f.StreamId)
	}

	if f.Version != c.version {
		return errStreamVersion{f.StreamId, f.Version}
	}

	if s.rxFinished {
		return errStreamAlreadyClosed(f.StreamId)
	}

	if f.Finished {
		s.rxLock.Lock()
		s.rxFinished = f.Finished
		s.rxLock.Unlock()
	}

	return nil
}

func (c *Connection) handleRstStream(d []byte) error {
	f, err := parseRstStream(d)
	if err != nil {
		return err
	}

	Log("rx RST_STREAM %+v\n", f)

	s := c.streams[f.StreamId]
	if s == nil {
		// ignore resets for closed streams
		return nil
	}

	err = errStreamProtocol(f.StreamId)
	switch f.Reason {
	case rstInvalidStream:
		err = errInvalidStream(f.StreamId)
	case rstRefusedStream:
		err = errRefusedStream(f.StreamId)
	case rstUnsupportedVersion:
		err = errStreamVersion{f.StreamId, c.version}
	case rstCancel:
		err = errCancel(f.StreamId)
	case rstFlowControlError:
		err = errStreamFlowControl(f.StreamId)
	case rstStreamInUse:
		err = errStreamInUse(f.StreamId)
	case rstStreamAlreadyClosed:
		err = errStreamAlreadyClosed(f.StreamId)
	}

	// Don't return an error and handle the error locally since we don't
	// want to send a RST_STREAM
	c.finishStream(s, err)
	return nil
}

func (c *Connection) handleSettings(d []byte) error {
	f, err := parseSettings(d)
	if err != nil {
		return err
	}

	Log("rx SETTINGS %+v\n", f)

	if f.Version != c.version {
		return errSessionVersion(f.Version)
	}

	if !f.HaveWindow {
		return nil
	}

	change := f.Window - c.rxWindow
	c.rxWindow = f.Window

	for _, s := range c.streams {
		s.txLock.Lock()
		s.txWindow += change
		s.txCond.Broadcast()
		s.txLock.Unlock()
	}

	return nil
}

func (c *Connection) handleWindowUpdate(d []byte) error {
	f, err := parseWindowUpdate(d)
	if err != nil {
		return err
	}

	Log("rx WINDOW_UPDATE %+v\n", f)

	s := c.streams[f.StreamId]
	if s == nil {
		return errInvalidStream(f.StreamId)
	}

	if f.Version != c.version {
		return errStreamVersion{f.StreamId, f.Version}
	}

	s.txLock.Lock()
	s.txWindow += f.WindowDelta
	s.txCond.Broadcast()
	s.txLock.Unlock()

	return nil
}

func (c *Connection) handlePing(d []byte) error {
	f, err := parsePing(d)
	if err != nil {
		return err
	}

	Log("rx PING %+v\n", f)

	if f.Version != c.version {
		return errSessionVersion(f.Version)
	}

	// Ignore loopback pings
	if (f.Id & 1) != (c.nextPingId & 1) {
		c.sendControl <- &pingFrame{
			Version: c.version,
			Id:      f.Id,
		}
	}

	return nil
}

func (c *Connection) handleGoAway(d []byte) error {
	f, err := parseGoAway(d)
	if err != nil {
		return err
	}

	Log("rx GO_AWAY %+v\n", f)

	if f.Version != c.version {
		return errSessionVersion(f.Version)
	}

	// This is so we don't start any streams after this point, and
	// finishStream will detect once we've finished all the active streams
	// and shut down the socket.
	c.goAway = true
	close(c.onGoAway)

	for id, s := range c.streams {
		err := errSessionProtocol

		switch f.Reason {
		case rstSuccess:
			err = errGoAway
		case rstUnsupportedVersion:
			err = errSessionVersion(c.version)
		case rstFlowControlError:
			err = errSessionFlowControl
		}

		// Reset all streams that we started which are after the last
		// accepted stream
		if id > f.LastStreamId && (id&1) == (c.nextStreamId&1) {
			c.finishStream(s, err)
		}
	}

	return nil
}

func (c *Connection) handleData(d []byte) error {
	f, err := parseData(d)
	if err != nil {
		return err
	}

	Log("rx DATA &{StreamId:%d Finished:%v Data:len %d}\n", f.StreamId, f.Finished, len(f.Data))

	s := c.streams[f.StreamId]
	if s == nil {
		return errInvalidStream(f.StreamId)
	}

	if s.rxFinished {
		return errStreamAlreadyClosed(f.StreamId)
	}

	// The rx pump thread could not give us the entire message due to it
	// being too large.
	if length := int(fromBig32(d[4:]) & 0xFFFFFF); length != len(f.Data) {
		return errStreamFlowControl(f.StreamId)
	}

	s.rxLock.Lock()
	s.rxBuffer.Write(f.Data)
	s.rxFinished = f.Finished
	s.rxCond.Broadcast()
	s.rxLock.Unlock()

	return nil
}

func (c *Connection) handleFrame(d []byte, unzip *decompressor) error {
	code := fromBig32(d[0:])

	if code&0x80000000 == 0 {
		return c.handleData(d)
	}

	if length := int(fromBig32(d[4:]) & 0xFFFFFF); length+8 != len(d) {
		return errSessionFlowControl
	}

	switch code & 0x8000FFFF {
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

	case goAwayCode:
		return c.handleGoAway(d)
	}

	// Messages with unknown type are ignored.
	return nil
}

// NewConnection creates a SPDY client or server connection around sock.
//
// sock should be the underlying socket already connected. Typically this is a
// TLS connection which has already gone through next protocol negotiation,
// but any socket will work.
//
// Handler is used to provide the callback for any content pushed from the
// server. If it is nil then pushed streams are refused.
//
// The connection won't be started until you run Connection.Run()
func NewConnection(sock net.Conn, handler http.Handler, version int, server bool) *Connection {
	c := &Connection{
		socket:           sock,
		version:          version,
		handler:          handler,
		remoteAddr:       sock.RemoteAddr(),
		rxWindow:         defaultWindow,
		sendControl:      make(chan frame, 100),
		sendWindowUpdate: make(chan frame, 100),
		dataSent:         make(chan error),
		onStartRequest:   make(chan *stream),
		onRequestStarted: make(chan error),
		onStreamFinished: make(chan *stream),
		streams:          make(map[int]*stream),
		lastStreamOpened: 0,
		onGoAway:         make(chan bool),
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
