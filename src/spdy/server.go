
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
func (c *connection) txPump(sock io.WriteCloser, control chan frame, data []chan frame) {
	zip := compressor{}

	for {
		f := nextTxFrame(control, data)

		if err := f.WriteTo(c.socket, &zip); err != nil {
			break
		}
	}

	c.socket.Close()
}

// rxPump runs the connection receive loop for both client and server
// connections. It then finds the message boundaries and sends each one over
// to the connection thread.
func (c *connection) rxPump(sock io.ReadCloser, dispatch chan []byte, dispatched chan os.Error) {

	go c.txThread()
	go c.dispatchThread()

	buf := new(buffer)
	unzip := decompressor{}

	for {
		d, err := buf.Get(sock, 8)
		if err != nil {
			goto end
		}

		length := (fromBig32(d[4:]) & 0xFFFFFF) + 8

		d, err := buf.Get(sock, length)
		// If we get an error due to the buffer overflowing, then
		// len(d) < 8 + length. We try and continue anyways, and the
		// disptach thread can decide whether we need to throw a
		// session error and disconnect or just send a stream error.
		if err != nil {
			goto end
		}

		dispatch <- d
		err := <-dispatched

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
func (c *connection) run(sock io.ReadWriteCloser, fallback chan net.Conn) {
	var err os.Error
	unzip = &decompressor{}

	if tlsConn, ok := c.socket.(*tls.Conn); ok {
		if err := tlsConn.Handshake(); err != nil {
			return
		}

		c.tlsState = new(tls.ConnectionState)
		*c.tlsState = tlsConn.ConnectionState()

		// Hand the connection off to the standard HTTPS server
		if fallback != nil && !strings.HasPrefix(c.tlsState.NegotiatedProtocol, "spdy/") {
			fallback <-c.socket
			return
		}
	}

	dispatch := make(chan []byte)
	dispatched := make(chan os.Error)

	go txPump(sock, c.sendControl, c.sendData)
	go rxPump(sock, dispatch, dispatched)

	for {
		select {
		case s := <-c.startRequest:
			s.streamId = c.nextStreamId

			if uint(s.streamId) > maxStreamId {
				c.onStreamFinished(s, ErrTooManyStreams)
				break
			}

			c.nextStreamId += 2

			if s.parent != nil {
				s.parent.children = append(s.parent.children, s)
				f.AssociatedStreamId = s.parent.streamId
			}

			// note we always use the control channel to ensure that the
			// SYN_STREAM packets are sent out in the order in which the stream
			// ids were allocated
			c.sendControl <- synStreamFrame{
				StreamId: s.streamId,
				Finished: s.txClosed,
				Unidirectional: unidirectional,
				Header: req.Header,
				Priority: priority,
				URL: req.URL,
				Proto: req.Proto,
				Method: req.Method,
			}

			// unidirectional and immediate finish messages never
			// get added to the streams table
			if !s.txFinished || !s.rxFinished {
				c.streams[s.streamId] = s
			}

		case s := <-c.onFinishedSent:
			s.txFinished = true
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


// startConnection allocates and starts a new connection wrapping around the
// provided socket.
func startConnection(sock net.Conn, handler http.Handler, fallback chan net.Conn, server bool) *connection {
	c := &connection {
		sendControl: make(chan *stream),
		streams: make(map[int]*stream),
		handler: handler,
		remoteAddr: sock.RemoteAddr(),
		window: defaultWindow,
		txFinished: make(chan bool),
	}

	if server {
		c.nextStreamId = 2
		c.nextPingId = 0
	} else {
		c.nextStreamId = 1
		c.nextPingId = 1
	}

	go c.run(sock, fallback)
	return c
}

// NewConnection creates a SPDY client connection around sock.
//
// sock should be the underlying socket already connected. Typically this is a
// TLS connection which has already gone the next protocol negotiation, but
// any socket will work.
//
// Handler is used to provide the callbacks for any content pushed from the
// server. If it is nil then pushed streams are refused.
func NewConnection(sock net.Conn, handler http.Handler) RoundTripper {
	return startConnection(sock, handler, nil, false)
}

// requestTxThread pushes the request body down the stream
func requestTxThread(body io.ReadCloser, s *stream) {
	io.Copy(s, body)
	s.Close()
	body.Close()
}

// startRequest starts a new request and starts pushing the request body and
// waits for the reply (if non unidirectional).
func (c *connection) startRequest(req *http.Request, priority int, unidirectional bool, associated *stream) (resp *http.Response, err os.Error) {
	finished := req.Body == nil

	s := &stream{}
	s.cond = sync.NewCond(&s.lock)

	s.rxFinished = unidirectional
	s.txFinished = finished
	s.onFinishedSent = c.onFinishedSent

	s.txClosed = finished
	s.sendReply = false
	s.finishedSent = finished
	s.txWindow = defaultWindow

	s.send = c.sendData[priority]

	s.forceTxError = make(chan bool)

	// Send the SYN_REQUEST
	c.startRequest <- s

	// Start the request body push
	if !finished {
		go requestTxThread(req.Body, s)
	}

	// Wait for the reply
	if unidirectional {
		return nil, nil
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	for s.response == nil && s.closeError != nil {
		s.cond.Wait()
	}
	req.Body = nil
	s.response.Request = req

	return s.response, s.closeError
}

// RoundTrip starts a new request on the connection and then waits for the
// response header.
//
// This can be safely called from any thread.
//
// To change the priority of the request set the ":priority" header field to a
// number between 0 (highest) and MaxPriority-1 (lowest). Otherwise
// DefaultPriority will be used.
//
// To start an unidirectional request where we do not wait for the response,
// set the ":unidirectional" header to a non empty value. The return value
// resp will then be nil.
func (c *connection) RoundTrip(req *http.Request) (resp *http.Response, err os.Error) {
	priority := DefaultPriority
	if p, err := strconv.Atoi(req.Header.Get(":priority")); err != nil && 0 <= p && p < MaxPriority {
		priority = p
	}

	unidirectional := len(req.Header.Get(":unidirectional")) > 0

	s := startRequest(req, priority, unidirectional, nil)

	if unidirectional {
		return nil, nil
	}

	return &s.response, nil
}

// serve runs the server accept loop
func serve(addr string, listener net.Listener, handler http.Handler, fallback chan net.Conn) os.Error {

	if handler == nil {
		handler = http.DefaultServeMux
	}

	for {
		sock, err := listener.Accept()
		if err != nil {
			return err
		}

		startConnection(sock, handler, fallback, true)
	}

	panic("unreachable")
}

// ListenAndServe listens for unencrypted SPDY connections on addr.
//
// Because it does not use TLS/SSL of this it can't use the next protocol
// negotation in TLS to fall back on standard HTTP.
func ListenAndServe(addr string, handler http.Handler) os.Error {
	if addr == "" {
		addr = ":http"
	}
	conn, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return serve(addr, conn, handler)
}


// ListenAndServeTLS listens for encrpyted SPDY or HTTPS connections on addr.
//
// It uses the TLS next negotation protocol to fallback on standard https.
func ListenAndServerTLS(addr string, certFile string, keyFile string, handler http.Handler) os.Error {
	if addr == "" {
		addr = ":https"
	}
	cfg := &tls.Config{
		Rand: rand.Reader,
		Time: time.Seconds,
		NextProtos: []string{"spdy/2.0", "http/1.1"},
		Certificates: make([]tls.Certificate, 1),
	}

	cfg.Certificates[0], err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}

	conn, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	tlsListener := tls.NewListener(conn, cfg)

	fallback := &httpsListener{
		error: make(chan os.Error),
		accept: make(chan net.Conn),
		addr: listener.Addr(),
	}

	go http.Server{Addr: addr, Handler: handler}.Serve(fallback)

	err := serve(tlsListener, handler, fallback.accept)
	fallback.error <- err
	return err
}

// httpsListener is a fake listener for feeding to the standard HTTPS server.
//
// This is so that we can hand it connections which negotiate https as their
// protocol through TLS next protocol negotation.
type httpsListener struct {
	error chan os.Error
	accept chan net.Conn
	addr Addr
}

func (s *httpsListener) Accept() (net.Conn, os.Error) {
	select {
	case err := <-s.error:
		return nil, err
	case sock := <-s.accept:
		return sock, nil
	}
	panic("unreachable")
}

func (s *httpsListener) Close() os.Error {
	return nil
}

func (s *httpsListener) Addr() Addr {
	return s.addr
}

