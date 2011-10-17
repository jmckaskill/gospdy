
const (
	maxPriorities = 8
	defaultBufferSize = 64 * 1024
	defaultWindow = 64 * 1024
)

type Pusher interface {
	Push(r *http.Request, unidirectional bool)
}

// data in connections are only accessible on the connection dispatch thread
type connection struct {
	// general connection info
	handler http.Handler
	remoteAddr string
	tlsState tls.ConnectionState
	rxWindow int

	// tx thread channels
	sendControl chan frame
	sendData [maxPriorities]chan frame

	// dispatch thread channels
	startRequest chan *stream
	onFinishedSent chan *stream // channel to notify the dispatch thread that FLAG_FIN has been sent

	// stream info
	streams map[int]*stream
	lastStreamOpened int
	nextStreamId int

	nextPingId uint32
}

type stream struct {
	streamId uint32

	lock sync.Mutex
	cond *sync.Cond // trigger when locked data is changed

	// access is locked
	response *http.Response

	// access is locked, write from dispatch, read on stream rx
	rxFinished bool // frame with FLAG_FIN has been received
	rxBuffer *bytes.Buffer

	txFinished bool // dispatch thread has been notified that a frame with FLAG_FIN has been sent
	onFinishedSent chan *stream // channel to notify the dispatch thread that FLAG_FIN has been sent

	// access from stream tx thread
	txClosed bool // streamTx.Close has been called
	sendReply bool // if SYN_REPLY still needs to be sent
	headerWritten bool // streamTx.WriteHeader has been called
	finishedSent bool // frame with FLAG_FIN has been sent

	txWindow int // access is locked, session rx and stream tx threads
	txBuffer *bufio.Buffer

	// channel used to send data and reply frames
	send [maxPriorities]chan frame
	priority int

	error os.Error // write access is locked
	forceTxError chan bool

	children []*stream // access is locked by streamLock
	parent *stream
}

// onStreamFinished removes a completed stream.
//
// It is called when:
// a) we transmit or receive a RST_STREAM
// b) grabStreamLock where we notice that both rx and tx have been closed
// c) when the rx closes if stream.Close has already been called
//
// These are all on the session rx thread
//
// err is nil for successfull completion (b and c) and non nil for aborts (a).
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
		c.streams[s.StreamId] = nil, false
	}

	// Disconnect child streams
	for _, a := range s.children {
		c.onStreamFinished(a, ErrAssociatedStreamClosed)
	}

	s.lock.Lock()
	s.rxFinished = true
	s.txFinished = true
	s.error = err
	close(s.forceTxError)
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
	// also check c.streams to handle the case where the stream was
	// aborted right after it finished
	if s.txFinished && s.txFinished && c.streams[s.StreamId] == s {
		c.onStreamFinished(s, err)
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
	if s2 := s.streams[f.StreamId]; s2 != nil {
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

	s = new(stream)

	s.streamId = f.StreamId
	s.cond = sync.NewCond(&s.lock)

	s.rxFinished = f.Finished
	s.txFinished = f.Unidirectional
	s.onFinishedSent = c.onFinishedSent

	s.txClosed = f.Unidirectional
	s.sendReply = !f.Unidirectional
	s.finishedSent = f.Unidirectional

	s.txWindow = defaultWindow

	s.send = c.sendData
	s.priority = f.Priority
	s.forceTxError = make(chan bool)

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
		Method: f.Method,
		RawURL: f.Path,
		URL: f.URL,
		Proto: f.Proto,
		Header: f.Header,
		Body: (*streamRx)(s),
		ContentLength: strconv.Atoi(f.Header.Get("Content-Length")),
		TransferEncoding: []string{},
		Close: true,
		Host: f.URL.Host,
		RemoteAddr: c.remoteAddr,
		TLS: &c.tlsState,
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
		c.sendControl <- rstStreamFrame{f.StreamId, rstAlreadyInUse}
		c.onStreamFinished(s, ErrAlreadyInUse)
		return nil
	}

	if s.rxClosed {
		c.sendControl <- rstStreamFrame{f.StreamId, rstAlreadyClosed}
		c.onStreamFinished(s, ErrAlreadyClosed)
		return nil
	}

	r := &http.Response{
		Method: f.Method,
		RawURL: f.Path,
		URL: f.URL,
		Proto: f.Proto,
		Header: f.Header,
		Body: s,
		ContentLength: strconv.Atoi(f.Header.Get("Content-Length")),
		TransferEncoding: []string{},
		Close: true,
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

	s := s.streams[f.StreamId]
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

	s := s.streams[f.StreamId]
	if s == nil {
		// ignore resets for closed streams
		return nil
	}

	err := ErrProtocolError
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

	change := f.Window - c.window
	c.window = f.Window

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
	s.txWindow += c.Delta
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
		c.sendControl <- rstStreamFrame(f.StreamId, rstInvalidStream}
		return nil
	}

	if s.rxClosed {
		c.sendControl <- rstStreamFrame(f.StreamId, rstAlreadyClosed}
		c.onStreamReset(s, ErrAlreadyClosed)
		return nil
	}

	// The rx pump thread could not give us the entire message due to it
	// being too large.
	if length := fromBig32(d[4:]) & 0xFFFFFF; length != len(f.Data) {
		c.sendControl <- rstStreamFrame{f.StreamId, rstFlowControlError}
		c.onStreamReset(s, ErrFlowControl)
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
		s.rxClosed = true
		c.checkStreamFinished(s)
	} else {
		c.sendControl <- windowUpdateFrame{s.StreamId, len(f.Data)}
	}

	return nil
}

func (c *connection) handleFrame(d []byte, unzip *decompressor) os.Error {
	code := uint(fromBig32(d[0:]))

	if code & 0x80000000 == 0 {
		return c.handleData(d)
	}

	if length := fromBig32(d[4:]) & 0xFFFFFF; length != len(d) {
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

	case headerCode:
		return c.handleHeaders(d, unzip)
	}

	ver := (code >> 16) & 0x7FFF
	ctype := code & 0xFFFF

	// Reply with unsupported version for SYN_STREAM. This should be
	// sufficient as its the first message sent to open a stream. We can't
	// cover the version in the general case, because we don't know what
	// stream id to abort.
	if ver != version && ctype == synStreamControlType && len(d) > 12 {
		streamId := fromBig32(d[8:])
		c.sendControl <- rstStreamFrame(streamId, rstUnsupportedVersion}
	}

	// Messages with the correct version but unknown type are ignored.

	return nil
}




// Split the user accessible stream methods into seperate sets

// Used as the txBuffer output
type streamTxOut stream

// Given to the user when they can write
type streamTx stream

// Given to the user when the can read
type streamRx stream


// Read reads request/response data.
//
// This is called by the resp.Body.Read by the user after starting a request.
//
// It is also called by the user to get request data in request.Body.Read.
//
// This will return os.EOF when all data has been successfully read without
// getting a SPDY RST_STREAM (equivalent of an abort).
func (s *streamRx) Read(buf []byte) (int, os.Error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	for !s.rxFinished && (s.rxBuffer == nil || s.rxBuffer.Len() == 0) && s.closeError == nil {
		s.cond.Wait()
	}

	if s.closeError != nil {
		return 0, s.closeError
	}

	if s.rxBuffer == nil {
		return 0, os.EOF
	}

	// This returns os.EOF if we read with no data due to s.rxClosed ==
	// true
	return s.rxBuffer.Read(buf)
}

// Closes the rx channel
//
// TODO(james): Currently we do nothing. Should we send a refused or cancel?
func (s *streamRx) Close() os.Error {
	return nil
}

// Header returns the response header so that headers can be changed.
//
// The header should not be altered after WriteHeader or Write has been
// called.
func (s *streamTx) Header() http.Header {
	return s.replyHeader
}

// WriteHeader writes the response header.
//
// The header will be buffered until the next Flush, the handler function
// returns or when the tx buffer fills up.
//
// The Header() should not be changed after calling this.
func (s *streamTx) WriteHeader(status int) os.Error {
	if s.txClosed {
		return ErrSendAfterClose
	}

	s.headerWritten = true
	s.status = status
	return nil
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
func (s *streamTx) Write(data []byte) (int, os.Error) {
	if s.txClosed {
		return ErrSendAfterClose
	}

	if !s.headerWritten {
		if err := s.WriteHeader(http.StatusOk); err != nil {
			return 0, err
		}
	}

	if s.txBuffer == nil {
		s.txBuffer = bufio.NewWriter((*streamTxOut)(s))
	}

	return s.txBuffer.Write(data)
}

// Close closes the tx pipe and flushes any buffered data.
func (s *streamTx) Close() os.Error {
	if s.txClosed {
		return ErrAlreadyClosed
	}

	s.txClosed = true

	if err := s.Flush(); err != nil {
		return err
	}

	// In most cases the close will have already been sent with the last
	// of the data as it got flushed through, but in cases where no data
	// was buffered (eg if it already been flushed or we never sent any)
	// then we send an empty data frame with the finished flag here.
	if !s.finishedSent {
		return nil
	}

	f := dataFrame{
		Finished: true,
		StreamId: s.streamId,
	}

	if err := s.sendFrame(f); err != nil {
		return err
	}

	s.finishedSent = true
	s.onFinishedSent <- s
	return nil

}

// Flush flushes data being written to the sessions tx thread which flushes it
// out the socket.
func (s *streamTx) Flush() os.Error {
	if !s.replySent {
		if err := s.sendReply(); err != nil {
			return err
		}
	}

	if s.txBuffer != nil {
		if err := s.txBuffer.Flush(); err != nil {
			return err
		}
	}

	return nil
}

// sendFrame sends a frame to the session tx thread, which sends it out the
// socket.
func (s *stream) sendFrame(f Frame) os.Error {
	select {
	case <-s.forceTxError:
		return s.error
	case s.send <- f:
	}

	return nil
}

// sendReply sends the SYN_REPLY frame which contains the response headers.
// Note this won't be called until the first flush or the tx channel is closed.
func (s *stream) sendReply() os.Error {
	s.replySent = true
	f := synReplyFrame{
		Finished: s.txClosed,
		StreamId: s.streamId,
		Header: s.replyHeader,
	}

	if f.Finished {
		s.finishedSent = true
		s.onFinishedSent <- s
	}

	return s.sendFrame(f)
}

// amountOfDataToSend figures out how much data we can send, potentially
// waiting for a WINDOW_UPDATE frame from the remote. It only returns once we
// can send > 0 bytes or the remote sent a RST_STREAM to abort.
func (s *streamTxOut) amountOfDataToSend() (int, os.Error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	for s.txWindow <= 0 && s.closeError != nil {
		s.cond.Wait()
	}

	if s.closeError != nil {
		return 0, s.closeError
	}

	tosend := len(data)
	if tosend < s.txWindow {
		tosend = s.txWindow
	}

	s.txWindow -= tosend
	return tosend, nil
}

// Function hooked up to the output of s.txBuffer to flush data to the session
// tx thread.
func (s *streamTxOut) Write(data []byte) (int, os.Error) {
	// If this is the first call and is due to the tx buffer filling up,
	// then the reply hasn't yet been sent.
	if !s.replySent {
		if err := s.sendReply(); err != nil {
			return 0, err
		}
	}

	sent := 0
	for sent < len(data) {
		tosend, err = s.amountOfDataToSend()
		if err != nil {
			return sent, err
		}

		f := dataFrame{
			Finished: s.txClosed && sent+tosend == len(data)
			Data: data[sent:sent+tosend],
			StreamId: s.streamId,
		}

		if err := s.sendFrame(f); err != nil {
			return sent, err
		}

		if f.Finished {
			s.finishedSent = true
			s.onFinishedSent <- s
		}

		sent += tosend
	}

	return sent, nil
}

