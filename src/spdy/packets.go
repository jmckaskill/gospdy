
const (
	version = 3

	synStreamControlType = 1

	synStreamCode = (1 << 31) | (version << 16) | 1
	synReplyCode = (1 << 31) | (version << 16) | 2
	rstStreamCode = (1 << 31) | (version << 16) | 3
	settingsCode = (1 << 31) | (version << 16) | 4
	pingCode = (1 << 31) | (version << 16) | 6
	goAwayCode = (1 << 31) | (version << 16) | 7
	headersCode = (1 << 31) | (version << 16) | 8
	windowUpdateCode = (1 << 31) | (version << 16) | 9

	finishedFlag = 1
	compressFlag = 2
	unidirectionalFlag = 2

	windowSetting = 5

	headerDictionary = `optionsgetheadpostputdeletetraceacceptaccept-charsetaccept-encodingaccept-languageauthorizationexpectfromhostif-modified-sinceif-matchif-none-matchif-rangeif-unmodifiedsincemax-forwardsproxy-authorizationrangerefererteuser-agent100101200201202203204205206300301302303304305306307400401402403404405406407408409410411412413414415416417500501502503504505accept-rangesageetaglocationproxy-authenticatepublicretry-afterservervarywarningwww-authenticateallowcontent-basecontent-encodingcache-controlconnectiondatetrailertransfer-encodingupgradeviawarningcontent-languagecontent-lengthcontent-locationcontent-md5content-rangecontent-typeetagexpireslast-modifiedset-cookieMondayTuesdayWednesdayThursdayFridaySaturdaySundayJanFebMarAprMayJunJulAugSepOctNovDecchunkedtext/htmlimage/pngimage/jpgimage/gifapplication/xmlapplication/xhtmltext/plainpublicmax-agecharset=iso-8859-1utf-8gzipdeflateHTTP/1.1statusversionurl` + "\x00"
	compressionLevel = zlib.DefaultCompression

	rstProtocolError = 1
	rstInvalidStream = 2
	rstRefusedStream = 3
	rstUnsupportedVersion = 4
	rstCancel = 5
	rstFlowControlError = 6
	rstStreamInUse = 7
	rstStreamAlreadyClosed = 8
)

var parseError = os.NewError("parse error")

func toBig32(d []byte, val int) {
	d[0] = byte(val >> 24)
	d[1] = byte(val >> 16)
	d[2] = byte(val >> 8)
	d[3] = byte(val)
}

func fromBig32(d []byte) int {
	return int(d[0] << 24) |
		int(d[1] << 16) |
		int(d[2] << 8) |
		int(d[3]
}

type decompressor struct {
	data []byte
	buf *bytes.Buffer
	zlib io.ReadCloser
}

func (s *decompressor) Read(buf []byte) (n int, err os.Error) {
	copy(buf, s.data)
	n = len(buf)
	if len(s.data) < len(buf) {
		n = len(s.data)
	}
	s.data = s.data[n:]
	if n == 0 {
		err = os.EOF
	}
	return
}

func (s *decompressor) Decompress(data []byte) (http.Header, os.Error) {
	if s.buf == nil {
		s.buf = bytes.NewBuffer()
		s.zlib = zlib.NewReaderDict(s, []byte(headerDictionary))
	} else {
		s.buf.Reset()
	}

	s.data = data
	io.Copy(s.zlib, s.buf)

	h := s.buf.Buffer()
	headers := make(http.Header)
	for {
		if len(h) < 4 {
			return nil, parseError
		}

		klen := fromBig32(h)
		h = h[4:]

		if len(h) < klen + 4 || klen < 0 {
			return nil, parseError
		}

		key := string(h[:klen])
		vlen := fromBig32(h)
		h = h[4:]

		if len(h) < vlen || vlen < 0 {
			return nil, parseError
		}

		val := h[:vlen]
		h = h[vlen:]
		vals := make([]byte, 0)

		for len(val) > 0 {
			nul := bytes.IndexByte(val, '\x00')
			if nul < 0 {
				vals = append(vals, string(val))
				break
			} else if nul == 0 {
				val = val[1:]
			} else {
				vals = append(vals, string(val[:nul]))
				val = val[nul+1:]
			}
		}

		headers[key] = vals
	}

	return headers, nil
}

type compressor struct {
	buf *bytes.Buffer
	zlib *zlib.Writer
}

func (s *compressor) Begin(init []byte) {
	if buf == nil {
		s.buf = bytes.NewBuffer()
		s.zlib = zlib.NewWriter(s.buf, compressionLevel, []byte(headerDictionary))
	} else {
		s.buf.Reset()
	}

	s.buf.Write(init)
}

func (s *compressor) Compress(key string, val string) {
	var k, v [4]byte
	toBig32(k[:], len(key))
	toBig32(v[:], len(val))
	s.zlib.Write(k[:])
	s.zlib.Write([]byte(key))
	s.zlib.Write(v[:])
	s.zlib.Write([]byte(val))
}

func (s *compressor) CompressMulti(init []byte, headers http.Header) {
	for key, val := range headers {
		if len(key) > 0 && key[0] != ":" {
			var k,v [4]byte

			toBig32(k[:], len(key))
			s.zlib.Write(k[:])
			s.zlib.Write(bytes.ToLower([]byte(key)))

			vals = strings.Join(val, "\x00")
			toBig32(v[:], len(vals))
			s.zlib.Write(v[:])
			s.zlib.Write([]byte(vals))
		}
	}
}

func (s *compressor) Finish() []byte {
	s.zlib.Flush()
	return s.buf.Bytes()
}

type packet interface {
	Send(w io.Writer, c *decompressor) os.Error
}

type synStreamFrame struct {
	Finished bool
	Unidirectional bool
	StreamId int
	AsssociatedStreamId int
	Header http.Header
	Priority byte
	URL *http.URL
	Proto string
	Method string
}

func (s *synStreamFrame) Send(w io.Writer, c *compressor) os.Error {
	flags := 0
	if s.Finished {
		flags |= finishedFlag << 24
	}
	if s.Unidirectional {
		flags |= unidirectionalFlag << 24
	}

	// TODO(james): error on not allowed headers (eg Connection)

	c.Begin([18]byte{}[:])
	c.Compress(":version", s.Proto)
	c.Compress(":method", s.Method)
	c.Compress(":path", s.URL.RawPath)
	c.Compress(":host", s.URL.Host)
	c.Compress(":scheme", s.URL.Scheme)
	c.CompressMulti(s.Header)
	h := c.Finish()
	toBig32(h[0:], synStreamCode)
	toBig32(h[4:], flags | (len(h) - 8))
	toBig32(h[8:], s.StreamId)
	toBig32(h[12:], s.AssociatedStreamId)
	h[16] = s.Priority << 5
	h[17] = 0 // unused

	return w.Write(h2)
}

func parseSynStream(d []byte, c *decompressor) (s synStreamFrame, err os.Error) {
	if len(d) < 18 {
		return s, parseError
	}
	s.Finished = (d[4] & finishedFlag) != 0
	s.Unidirectional = (d[4] & unidirectionalFlag) != 0
	s.StreamId = fromBig32(d[8:])
	s.AssociatedStreamId = fromBig32(d[12:])
	s.Priority = d[16] >> 5

	if s.Header, err = c.Decompress(d[18:]); err != nil {
		return s, err
	}

	s.Proto = s.Header.Get(":version")
	s.Method = s.Header.Get(":method")
	u := &http.URL{
		Scheme: s.Header.Get(":scheme"),
		Host: s.Header.Get(":host"),
		Path := s.Header.Get(":path"),
	}

	if q := strings.FindByte(u.Path, '?'); q > 0 {
		u.Query = u.Path[q+1:]
		u.Path = u.Path[:q]
	}

	if len(s.Proto) == 0 ||
		len(s.Method) == 0 ||
		len(u.Scheme) == 0 ||
		len(u.Host) == 0 ||
		len(u.Path) == 0 ||
		u.Path[0] != '/' {

		return parseError
	}

	// TODO(james): error on not allowed headers (eg Connection)

	u.Raw = u.String()
	s.URL = u

	return s, err
}

type synReplyFrame struct {
	Finished bool
	StreamId int
	Header http.Header
	Status string
	Proto string
}

func (s *synReplyFrame) Send(w io.Writer, c *compressor) os.Error {
	flags := 0
	if s.Finished {
		flags |= finishedFlag << 24
	}

	// TODO(james): error on not allowed headers (eg Connection)

	c.Begin([12]byte{}[:])
	c.Compress(":status", s.Status)
	c.Compress(":version", s.Proto)
	c.CompressMulti(s.Header)
	h := c.Finish()
	toBig32(h[0:], synReplyCode)
	toBig32(h[4:], flags | (len(h) - 8))
	toBig32(h[8:], s.StreamId)

	return w.Write(h)
}

func parseSynReply(d []byte, c *decompressor) (s synReplyFrame, err os.Error) {
	if len(d) < 12 {
		return s, parseError
	}
	s.Finished = (d[4] & finishedFlag) != 0
	s.StreamId = fromBig32(d[8:])
	if s.Header, err = c.Decompress(d[12:]); err != nil {
		return s, err
	}

	s.Status = s.Header.Get(":status")
	s.Proto = s.Header.Get(":version")
	if len(s.Status) == 0 || len(s.Proto) == 0 {
		return s, parseError
	}

	// TODO(james): error on not allowed headers (eg Connection)

	return s, nil
}

type headersFrame struct {
	Finished bool
	StreamId int
	Header http.Header
}

func (s *headersFrame) Send(w io.Writer, c *compressor) os.Error {
	flags := 0
	if s.Finished {
		flags |= finishedFlag << 24
	}

	c.Begin([12]byte{}[:])
	c.CompressMulti(s.Header)
	h := c.Finish()
	toBig32(h[0:], headersCode)
	toBig32(h[4:], flags | (len(h) - 8))
	toBig32(h[8:], s.StreamId)

	return w.Write(h)
}

func parseHeaders(d []byte, c *decompressor) (s headersFrame, err os.Error) {
	if len(d) < 12 {
		return s, parseError
	}

	s.Finished = (d[4] & finishedFlag) != 0
	s.StreamId = fromBig32(d[8:])
	if s.Header, err = c.Decompress(d[12:]); err != nil {
		return s, err
	}

	return s, nil
}

type rstStreamFrame struct {
	StreamId int
	Reason int
}

func (s *rstStreamFrame) Send(w io.Writer, c *compressor) os.Error {
	h := [16]byte{}
	toBig32(h[0:], rstStreamCode)
	toBig32(h[4:], 8) // length and no flags
	toBig32(h[8:], s.StreamId)
	toBig32(h[12:], s.Reason)
	return w.Write(h)
}

func parseRstStream(d []byte) (rstStreamFrame, os.Error) {
	if len(d) != 16 {
		return rstStreamFrame{}, parseError
	}
	return rstStreamFrame{
		StreamId: fromBig32(d[8:]),
		Reason: fromBig32(d[12:]),
	}, nil
}

type windowUpdateFrame struct {
	StreamId int
	WindowDelta int
}

func (s *windowUpdateFrame) Send(w io.Writer, c *compressor) os.Error {
	h := [16]byte{}
	toBig32(h[0:], windowUpdateCode)
	toBig32(h[4:], 8) // length and no flags
	toBig32(h[8:], s.StreamId)
	toBig32(h[12:], s.WindowDelta)
	return w.Write(h)
}

func parseWindowUpdate(d []byte) (windowUpdateFrame, os.Error) {
	if len(d) != 16 {
		return windowUpdateFrame{}, parseError
	}
	return windowUpdateFrame{
		StreamId: fromBig32(d[8:]),
		WindowDelta: fromBig32(d[12:]),
	}, nil
}

type settingsFrame struct {
	HaveWindow bool
	Window int
}

func (s *settingsFrame) Send(w io.Writer, c compressor) os.Error {
	if s.HaveWindow {
		h := [20]byte{}
		toBig32(h[0:], settingsCode)
		toBig32(h[4:], 12) // length from here and no flags
		toBig32(h[8:], 1) // number of entries
		toBig32(h[12:], windowSetting)
		toBig32(h[16:], s.Window)
		return w.Write(h)
	}

	return nil
}

func parseSettings(d []byte) (settingsFrame, os.Error) {
	var s settingsFrame

	if len(d) < 12 {
		return s, parseError
	}

	entries := fromBig32(d[8:])
	if len(d) - 12 != entries * 8 {
		return s, parseError
	}

	d = d[12:]
	for len(d) > 0 {
		key := fromBig32(d)
		val := fromBig32(d[4:])
		d = d[8:]

		if key == windowSetting {
			s.HaveWindow = true
			s.Window = val
		}
	}

	return ret, nil
}

type pingFrame struct {
	Id uint32
}

func (s *pingFrame) WriteTo(w io.Writer, c compressor) os.Error {
	h := [12]byte{}
	toBig32(h[0:], pingCode)
	toBig32(h[4:], 4) // length 4 and no flags
	toBig32(h[8:], s.Id)
	_, err := w.Write(h)
	return err
}

func parsePing(d []byte) (pingFrame, os.Error) {
	if len(d) != 12 {
		return nil, parseError
	}
	return pingFrame{fromBig32(d[8:])}, nil
}

type goAwayFrame struct {
	LastStreamId int
	Reason int
}

func (s *goAwayFrame) Send(w io.Writer, c *compressor) os.Error {
	h := [16]byte{}
	toBig32(h[0:], goAwayCode)
	toBig32(h[4:], 8) // length 8 and no flags
	toBig32(h[8:], s.LastStreamId)
	toBig32(h[12:], s.Reason)
	return nil
}

func parseGoAway(d []byte) (goAwayFrame, err os.Error) {
	if len(d) != 16 {
		return goAwayFrame{}, parseError
	}

	return goAwayFrame {
		LastStreamId: fromBig32(d[8:]),
		Reason: fromBig32(d[12:]),
	}, nil
}


type dataFrame struct {
	Data []byte
	StreamId int
	Finished bool
}

func (s *dataFrame) Send(w io.Writer, c compressor) os.Error {
	flags := 0
	if s.Finished {
		flags |= finishedFlag << 24
	}

	h := [8]byte{}
	toBig32(h[0:], s.StreamId)
	toBig32(h[4:], flags | len(s.Data))

	if _, err := w.Write(h); err != nil {
		return err
	}

	if _, err := w.Write(s.Data); err != nil {
		return err
	}

	return nil
}

func parseData(d []byte) (dataFrame, os.Error) {
	return dataFrame{
		StreamId: fromBig32(d[0:]),
		Finished: (d[4] & finishedFlag) != 0,
		Data: d[8:],
	}, nil
}

