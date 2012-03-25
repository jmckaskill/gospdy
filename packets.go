package spdy

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	LowPriority     = 3
	HighPriority    = -4
	DefaultPriority = 0

	synStreamCode    = (1 << 31) | 1
	synReplyCode     = (1 << 31) | 2
	rstStreamCode    = (1 << 31) | 3
	settingsCode     = (1 << 31) | 4
	noopCode         = (1 << 31) | 5
	pingCode         = (1 << 31) | 6
	goAwayCode       = (1 << 31) | 7
	headersCode      = (1 << 31) | 8
	windowUpdateCode = (1 << 31) | 9

	finishedFlag       = 1
	compressedFlag     = 2
	unidirectionalFlag = 2

	windowSetting = 5

	headerDictionaryV2 = `optionsgetheadpostputdeletetraceacceptaccept-charsetaccept-encodingaccept-languageauthorizationexpectfromhostif-modified-sinceif-matchif-none-matchif-rangeif-unmodifiedsincemax-forwardsproxy-authorizationrangerefererteuser-agent100101200201202203204205206300301302303304305306307400401402403404405406407408409410411412413414415416417500501502503504505accept-rangesageetaglocationproxy-authenticatepublicretry-afterservervarywarningwww-authenticateallowcontent-basecontent-encodingcache-controlconnectiondatetrailertransfer-encodingupgradeviawarningcontent-languagecontent-lengthcontent-locationcontent-md5content-rangecontent-typeetagexpireslast-modifiedset-cookieMondayTuesdayWednesdayThursdayFridaySaturdaySundayJanFebMarAprMayJunJulAugSepOctNovDecchunkedtext/htmlimage/pngimage/jpgimage/gifapplication/xmlapplication/xhtmltext/plainpublicmax-agecharset=iso-8859-1utf-8gzipdeflateHTTP/1.1statusversionurl` + "\x00"
	headerDictionaryV3 = `optionsgetheadpostputdeletetraceacceptaccept-charsetaccept-encodingaccept-languageauthorizationexpectfromhostif-modified-sinceif-matchif-none-matchif-rangeif-unmodifiedsincemax-forwardsproxy-authorizationrangerefererteuser-agent100101200201202203204205206300301302303304305306307400401402403404405406407408409410411412413414415416417500501502503504505accept-rangesageetaglocationproxy-authenticatepublicretry-afterservervarywarningwww-authenticateallowcontent-basecontent-encodingcache-controlconnectiondatetrailertransfer-encodingupgradeviawarningcontent-languagecontent-lengthcontent-locationcontent-md5content-rangecontent-typeetagexpireslast-modifiedset-cookieMondayTuesdayWednesdayThursdayFridaySaturdaySundayJanFebMarAprMayJunJulAugSepOctNovDecchunkedtext/htmlimage/pngimage/jpgimage/gifapplication/xmlapplication/xhtmltext/plainpublicmax-agecharset=iso-8859-1utf-8gzipdeflateHTTP/1.1statusversionurl`
	compressionLevel   = zlib.BestCompression

	rstSuccess             = 0
	rstProtocolError       = 1
	rstInvalidStream       = 2
	rstRefusedStream       = 3
	rstUnsupportedVersion  = 4
	rstCancel              = 5
	rstFlowControlError    = 6
	rstStreamInUse         = 7
	rstStreamAlreadyClosed = 8
)

func toBig16(d []byte, val uint16) {
	d[0] = byte(val >> 8)
	d[1] = byte(val)
}

func toBig32(d []byte, val uint32) {
	d[0] = byte(val >> 24)
	d[1] = byte(val >> 16)
	d[2] = byte(val >> 8)
	d[3] = byte(val)
}

func toLittle32(d []byte, val uint32) {
	d[0] = byte(val)
	d[1] = byte(val >> 8)
	d[2] = byte(val >> 16)
	d[3] = byte(val >> 24)
}

func fromBig16(d []byte) uint16 {
	return uint16(d[0])<<8 | uint16(d[1])
}

func fromBig32(d []byte) uint32 {
	return uint32(d[0])<<24 |
		uint32(d[1])<<16 |
		uint32(d[2])<<8 |
		uint32(d[3])
}

func fromLittle32(d []byte) uint32 {
	return uint32(d[0]) |
		uint32(d[1])<<8 |
		uint32(d[2])<<16 |
		uint32(d[3])<<24
}

type input struct {
	*bytes.Buffer
}

type decompressor struct {
	in  *bytes.Buffer
	out io.ReadCloser
}

func (s *decompressor) Decompress(streamId int, version int, data []byte) (headers http.Header, err error) {

	if s.in == nil {
		s.in = bytes.NewBuffer(nil)
	}

	s.in.Write(data)

	if s.out == nil {
		switch version {
		case 2:
			s.out, err = zlib.NewReaderDict(s.in, []byte(headerDictionaryV2))
		case 3:
			s.out, err = zlib.NewReaderDict(s.in, []byte(headerDictionaryV3))
		default:
			err = ErrStreamVersion{streamId, version}
		}

		if err != nil {
			return nil, err
		}
	}

	var numkeys int

	switch version {
	case 2:
		h := [2]byte{}
		if _, err := s.out.Read(h[:]); err != nil {
			return nil, err
		}
		numkeys = int(fromBig16(h[:]))
	case 3:
		h := [4]byte{}
		if _, err := s.out.Read(h[:]); err != nil {
			return nil, err
		}
		numkeys = int(fromBig32(h[:]))
	default:
		return nil, ErrStreamVersion{streamId, version}
	}

	headers = make(http.Header)
	for i := 0; i < numkeys; i++ {
		var klen, vlen int

		// Pull out the key

		switch version {
		case 2:
			h := [2]byte{}
			if _, err := s.out.Read(h[:]); err != nil {
				return nil, err
			}
			klen = int(fromBig16(h[:]))
		case 3:
			h := [4]byte{}
			if _, err := s.out.Read(h[:]); err != nil {
				return nil, err
			}
			klen = int(fromBig32(h[:]))
		default:
			return nil, ErrStreamVersion{streamId, version}
		}

		if klen < 0 || klen > defaultBufferSize {
			// TODO(james) new error as this isn't the whole packet data
			return nil, ErrParse(data)
		}

		key := make([]byte, klen)
		if _, err := s.out.Read(key); err != nil {
			return nil, err
		}

		// Pull out the value

		switch version {
		case 2:
			h := [2]byte{}
			if _, err := s.out.Read(h[:]); err != nil {
				return nil, err
			}
			vlen = int(fromBig16(h[:]))
		case 3:
			h := [4]byte{}
			if _, err := s.out.Read(h[:]); err != nil {
				return nil, err
			}
			vlen = int(fromBig32(h[:]))
		default:
			return nil, ErrStreamVersion{streamId, version}
		}

		if vlen < 0 || vlen > defaultBufferSize {
			// TODO(james) new error as this isn't the whole packet data
			return nil, ErrParse(data)
		}

		val := make([]byte, vlen)
		if _, err := s.out.Read(val); err != nil {
			return nil, err
		}

		// Split the value on nul boundaries
		for _, val := range bytes.Split(val, []byte{'\x00'}) {
			headers.Add(string(key), string(val))
		}
	}

	return headers, nil
}

type compressor struct {
	buf *bytes.Buffer
	w   *zlib.Writer
}

func (s *compressor) Begin(version int, init []byte, headers http.Header, numkeys int) (err error) {
	if s.buf == nil {
		s.buf = bytes.NewBuffer(make([]byte, 0, len(init)))
		s.buf.Write(init)

		switch version {
		case 2:
			s.w, err = zlib.NewWriterLevelDict(s.buf, compressionLevel, []byte(headerDictionaryV2))
		case 3:
			s.w, err = zlib.NewWriterLevelDict(s.buf, compressionLevel, []byte(headerDictionaryV3))
		default:
			err = ErrSessionVersion(version)
		}

		if err != nil {
			return err
		}

	} else {
		s.buf.Reset()
		s.buf.Write(init)
	}

	switch version {
	case 2:
		var keys [2]byte
		toBig16(keys[:], uint16(len(headers)+numkeys))
		s.w.Write(keys[:])

		for key, val := range headers {
			var k, v [2]byte
			vals := strings.Join(val, "\x00")

			toBig16(k[:], uint16(len(key)))
			s.w.Write(k[:])
			s.w.Write(bytes.ToLower([]byte(key)))

			toBig16(v[:], uint16(len(vals)))
			s.w.Write(v[:])
			s.w.Write([]byte(vals))
		}
	case 3:
		var keys [4]byte
		toBig32(keys[:], uint32(len(headers)+numkeys))
		s.w.Write(keys[:])

		for key, val := range headers {
			var k, v [4]byte

			toBig32(k[:], uint32(len(key)))
			s.w.Write(k[:])
			s.w.Write(bytes.ToLower([]byte(key)))

			vals := strings.Join(val, "\x00")
			toBig32(v[:], uint32(len(vals)))
			s.w.Write(v[:])
			s.w.Write([]byte(vals))
		}
	default:
		return ErrSessionVersion(version)
	}

	return nil
}

// TODO(james): what to do if len(key) or len(val) > UINT16_MAX or UINT32_MAX

func (s *compressor) CompressV2(key string, val string) {
	var k, v [2]byte
	toBig16(k[:], uint16(len(key)))
	toBig16(v[:], uint16(len(val)))
	s.w.Write(k[:])
	s.w.Write([]byte(key))
	s.w.Write(v[:])
	s.w.Write([]byte(val))
}

func (s *compressor) CompressV3(key string, val string) {
	var k, v [4]byte
	toBig32(k[:], uint32(len(key)))
	toBig32(v[:], uint32(len(val)))
	s.w.Write(k[:])
	s.w.Write([]byte(key))
	s.w.Write(v[:])
	s.w.Write([]byte(val))
}

func (s *compressor) Finish() []byte {
	s.w.Flush()
	return s.buf.Bytes()
}

type frame interface {
	WriteFrame(w io.Writer, c *compressor) error
}

func popHeader(h http.Header, key string) string {
	r := h.Get(key)
	h.Del(key)
	return r
}

type synStreamFrame struct {
	Version            int
	Finished           bool
	Unidirectional     bool
	StreamId           int
	AssociatedStreamId int
	Header             http.Header
	Priority           int
	URL                *url.URL
	Proto              string
	ProtoMajor         int
	ProtoMinor         int
	Method             string
}

var invalidSynStreamHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Connection",
	"Transfer-Encoding",
}

func (s *synStreamFrame) WriteFrame(w io.Writer, c *compressor) error {
	Log("spdy: tx SYN_STREAM %+v", *s)

	flags := uint32(0)
	if s.Finished {
		flags |= finishedFlag << 24
	}
	if s.Unidirectional {
		flags |= unidirectionalFlag << 24
	}

	if s.Header != nil {
		for _, key := range invalidSynStreamHeaders {
			delete(s.Header, key)
		}
	}

	if err := c.Begin(s.Version, make([]byte, 18), s.Header, 5); err != nil {
		return err
	}

	path := s.URL.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if s.URL.RawQuery != "" {
		path += "?" + s.URL.RawQuery
	}

	switch s.Version {
	case 2:
		c.CompressV2("version", s.Proto)
		c.CompressV2("method", s.Method)
		c.CompressV2("url", path)
		c.CompressV2("host", s.URL.Host)
		c.CompressV2("scheme", s.URL.Scheme)
	case 3:
		c.CompressV3(":version", s.Proto)
		c.CompressV3(":method", s.Method)
		c.CompressV3(":path", path)
		c.CompressV3(":host", s.URL.Host)
		c.CompressV3(":scheme", s.URL.Scheme)
	default:
		return ErrStreamVersion{s.StreamId, s.Version}
	}

	h := c.Finish()
	toBig32(h[0:], synStreamCode|uint32(s.Version<<16))
	toBig32(h[4:], flags|uint32(len(h)-8))
	toBig32(h[8:], uint32(s.StreamId))
	toBig32(h[12:], uint32(s.AssociatedStreamId))
	// Priority is 2 bits in V2, this works correctly in that case because
	// in V2 we don't use the bottom priority bit.
	h[16] = byte((s.Priority - HighPriority) << 5)
	h[17] = 0 // unused

	_, err := w.Write(h)
	return err
}

func parseSynStream(d []byte, c *decompressor) (*synStreamFrame, error) {
	if len(d) < 12 {
		Log("spdy: invalid SYN_STREAM length")
		return nil, ErrParse(d)
	}

	sid := int(fromBig32(d[8:]))

	if len(d) < 18 {
		Log("spdy: invalid SYN_STREAM length")
		return nil, ErrStreamProtocol(sid)
	} else if sid < 0 {
		Log("spdy: invalid stream id")
		return nil, ErrStreamProtocol(sid)
	}

	s := &synStreamFrame{
		Version:            int(fromBig16(d[0:]) & 0x7FFF),
		Finished:           (d[4] & finishedFlag) != 0,
		Unidirectional:     (d[4] & unidirectionalFlag) != 0,
		StreamId:           sid,
		AssociatedStreamId: int(fromBig32(d[12:])),
		Priority:           int(d[16]>>5) + HighPriority,
	}

	if s.AssociatedStreamId < 0 {
		Log("spdy: invalid associated stream id")
		return nil, ErrStreamProtocol(sid)
	}

	var err error
	if s.Header, err = c.Decompress(s.StreamId, s.Version, d[18:]); err != nil {
		Log("spdy: SYN_STREAM decompress error: %v", err)
		return nil, err
	}

	var scheme, host, path string

	switch s.Version {
	case 2:
		s.Proto = popHeader(s.Header, "Version")
		s.Method = popHeader(s.Header, "Method")
		scheme = popHeader(s.Header, "Scheme")
		host = popHeader(s.Header, "Host")
		path = popHeader(s.Header, "Url")
	case 3:
		s.Proto = popHeader(s.Header, ":version")
		s.Method = popHeader(s.Header, ":method")
		scheme = popHeader(s.Header, ":scheme")
		host = popHeader(s.Header, ":host")
		path = popHeader(s.Header, ":path")
	default:
		Log("spdy: SYN_STREAM unsupported version %d", s.Version)
		return nil, ErrStreamVersion{sid, s.Version}
	}

	var ok bool
	if s.ProtoMajor, s.ProtoMinor, ok = http.ParseHTTPVersion(s.Proto); !ok {
		Log("spdy: SYN_STREAM could not parse http version %s", s.Proto)
		return nil, ErrStreamProtocol(sid)
	}

	s.URL, err = url.Parse(fmt.Sprintf("%s://%s%s", scheme, host, path))
	if err != nil || strings.Index(scheme, ":") >= 0 || strings.Index(host, "/") >= 0 || len(path) == 0 || path[0] != '/' {
		Log("spdy: invalid SYN_STREAM url %s://%s%s: %v", scheme, host, path, err)
		return nil, ErrStreamProtocol(sid)
	}

	for _, key := range invalidSynStreamHeaders {
		if s.Header[key] != nil {
			Log("spdy: invalid SYN_STREAM header %s: %s", key, s.Header.Get(key))
			return nil, ErrStreamProtocol(sid)
		}
	}

	if len(s.Header) == 0 {
		s.Header = nil
	}

	return s, nil
}

type synReplyFrame struct {
	Version    int
	Finished   bool
	StreamId   int
	Header     http.Header
	Status     string
	Proto      string
	ProtoMajor int
	ProtoMinor int
}

var invalidSynReplyHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Connection",
	"Transfer-Encoding",
}

func (s *synReplyFrame) WriteFrame(w io.Writer, c *compressor) error {
	Log("spdy: tx SYN_REPLY %+v", *s)

	flags := uint32(0)
	if s.Finished {
		flags |= finishedFlag << 24
	}

	if s.Header != nil {
		for _, key := range invalidSynReplyHeaders {
			delete(s.Header, key)
		}
	}

	switch s.Version {
	case 2:
		if err := c.Begin(s.Version, make([]byte, 14), s.Header, 2); err != nil {
			return err
		}
		c.CompressV2("status", s.Status)
		c.CompressV2("version", s.Proto)
	case 3:
		if err := c.Begin(s.Version, make([]byte, 12), s.Header, 2); err != nil {
			return err
		}
		c.CompressV3(":status", s.Status)
		c.CompressV3(":version", s.Proto)
	default:
		return ErrSessionVersion(s.Version)
	}

	h := c.Finish()
	toBig32(h[0:], synReplyCode|uint32(s.Version<<16))
	toBig32(h[4:], flags|uint32(len(h)-8))
	toBig32(h[8:], uint32(s.StreamId))

	_, err := w.Write(h)
	return err
}

func parseSynReply(d []byte, c *decompressor) (*synReplyFrame, error) {
	if len(d) < 12 {
		return nil, ErrParse(d)
	}

	s := &synReplyFrame{
		Version:  int(fromBig16(d[0:]) & 0x7FFF),
		Finished: (d[4] & finishedFlag) != 0,
		StreamId: int(fromBig32(d[8:])),
	}

	if s.StreamId < 0 {
		return nil, ErrStreamProtocol(s.StreamId)
	}

	var err error
	switch s.Version {
	case 2:
		if len(d) < 14 {
			return nil, ErrStreamProtocol(s.StreamId)
		}
		if s.Header, err = c.Decompress(s.StreamId, s.Version, d[14:]); err != nil {
			return nil, err
		}
		s.Status = popHeader(s.Header, "Status")
		s.Proto = popHeader(s.Header, "Version")

	case 3:
		if s.Header, err = c.Decompress(s.StreamId, s.Version, d[12:]); err != nil {
			return nil, err
		}
		s.Status = popHeader(s.Header, ":status")
		s.Proto = popHeader(s.Header, ":version")

	default:
		return nil, ErrStreamVersion{s.StreamId, s.Version}
	}

	if len(s.Status) == 0 || len(s.Proto) == 0 {
		return nil, ErrStreamProtocol(s.StreamId)
	}

	var ok bool
	if s.ProtoMajor, s.ProtoMinor, ok = http.ParseHTTPVersion(s.Proto); !ok {
		return nil, ErrStreamProtocol(s.StreamId)
	}

	for _, key := range invalidSynReplyHeaders {
		if s.Header[key] != nil {
			Log("spdy: invalid SYN_REPLY header %s: %s", key, s.Header.Get(key))
			return nil, ErrStreamProtocol(s.StreamId)
		}
	}

	if len(s.Header) == 0 {
		s.Header = nil
	}

	return s, nil
}

type headersFrame struct {
	Version  int
	Finished bool
	StreamId int
	Header   http.Header
}

func (s *headersFrame) WriteFrame(w io.Writer, c *compressor) error {
	Log("spdy: tx HEADERS %+v", *s)

	flags := uint32(0)
	if s.Finished {
		flags |= finishedFlag << 24
	}

	switch s.Version {
	case 2:
		if err := c.Begin(s.Version, make([]byte, 14), s.Header, 0); err != nil {
			return err
		}
	case 3:
		if err := c.Begin(s.Version, make([]byte, 12), s.Header, 0); err != nil {
			return err
		}
	default:
		return ErrSessionVersion(s.Version)
	}

	h := c.Finish()
	toBig32(h[0:], headersCode|uint32(s.Version<<16))
	toBig32(h[4:], flags|uint32(len(h)-8))
	toBig32(h[8:], uint32(s.StreamId))

	_, err := w.Write(h)
	return err
}

func parseHeaders(d []byte, c *decompressor) (*headersFrame, error) {
	if len(d) < 12 {
		return nil, ErrParse(d)
	}

	s := &headersFrame{
		Version:  int(fromBig16(d) & 0x7FFF),
		Finished: (d[4] & finishedFlag) != 0,
		StreamId: int(fromBig32(d[8:])),
	}

	if s.StreamId < 0 {
		return s, ErrStreamProtocol(s.StreamId)
	}

	var err error
	switch s.Version {
	case 2:
		if len(d) < 16 {
			return nil, ErrStreamProtocol(s.StreamId)
		}
		if s.Header, err = c.Decompress(s.StreamId, s.Version, d[14:]); err != nil {
			return nil, err
		}
	case 3:
		if s.Header, err = c.Decompress(s.StreamId, s.Version, d[12:]); err != nil {
			return nil, err
		}
	default:
		return nil, ErrStreamVersion{s.StreamId, s.Version}
	}

	return s, nil
}

type rstStreamFrame struct {
	Version  int
	StreamId int
	Reason   int
}

func (s *rstStreamFrame) WriteFrame(w io.Writer, c *compressor) error {
	Log("spdy: tx RST_STREAM %+v", s)
	h := [16]byte{}
	toBig32(h[0:], rstStreamCode|uint32(s.Version<<16))
	toBig32(h[4:], 8) // length and no flags
	toBig32(h[8:], uint32(s.StreamId))
	toBig32(h[12:], uint32(s.Reason))
	_, err := w.Write(h[:])
	return err
}

func parseRstStream(d []byte) (*rstStreamFrame, error) {
	if len(d) < 12 {
		return nil, ErrParse(d)
	}

	sid := int(fromBig32(d[8:]))

	if len(d) < 16 || sid < 0 {
		return nil, ErrStreamProtocol(sid)
	}

	s := &rstStreamFrame{
		Version:  int(fromBig16(d) & 0x7FFF),
		StreamId: sid,
		Reason:   int(fromBig32(d[12:])),
	}

	if s.Reason == 0 {
		return nil, ErrStreamProtocol(sid)
	}

	return s, nil
}

type windowUpdateFrame struct {
	Version     int
	StreamId    int
	WindowDelta int
}

func (s *windowUpdateFrame) WriteFrame(w io.Writer, c *compressor) error {
	Log("spdy: tx WINDOW_UPDATE %+v", s)
	h := [16]byte{}
	toBig32(h[0:], windowUpdateCode|uint32(s.Version<<16))
	toBig32(h[4:], 8) // length and no flags
	toBig32(h[8:], uint32(s.StreamId))
	toBig32(h[12:], uint32(s.WindowDelta))
	_, err := w.Write(h[:])
	return err
}

func parseWindowUpdate(d []byte) (*windowUpdateFrame, error) {
	if len(d) < 12 {
		return nil, ErrParse(d)
	}

	sid := int(fromBig32(d[8:]))

	if len(d) < 16 || sid < 0 {
		return nil, ErrStreamProtocol(sid)
	}

	s := &windowUpdateFrame{
		Version:     int(fromBig16(d) & 0x7FFF),
		StreamId:    sid,
		WindowDelta: int(fromBig32(d[12:])),
	}

	if s.WindowDelta <= 0 {
		return nil, ErrStreamProtocol(sid)
	}

	return s, nil
}

type settingsFrame struct {
	Version    int
	HaveWindow bool
	Window     int
}

func (s *settingsFrame) WriteFrame(w io.Writer, c *compressor) error {
	if !s.HaveWindow {
		return nil
	}
	Log("spdy: tx SETTINGS %+v", s)

	h := [20]byte{}
	toBig32(h[0:], settingsCode|uint32(s.Version<<16))
	toBig32(h[4:], 12) // length from here and no flags
	toBig32(h[8:], 1)  // number of entries

	switch s.Version {
	case 2:
		// V2 has the window given in number of packets, but doesn't say how
		// big each packet is (we are going to use 1KB). It also has the key
		// in little endian.
		toLittle32(h[12:], windowSetting)
		toBig32(h[16:], uint32(s.Window/1024))
	case 3:
		toBig32(h[12:], windowSetting)
		toBig32(h[16:], uint32(s.Window))
	default:
		return ErrSessionVersion(s.Version)
	}

	_, err := w.Write(h[:])
	return err
}

func parseSettings(d []byte) (*settingsFrame, error) {
	if len(d) < 12 {
		return nil, ErrParse(d)
	}

	s := &settingsFrame{
		Version: int(fromBig16(d) & 0x7FFF),
	}

	entries := int(fromBig32(d[8:]))
	// limit the number of entries to some max boundary to prevent a
	// overflow when we calc the number of total bytes
	if entries < 0 || entries > 4096 {
		return nil, ErrParse(d)
	}

	if len(d)-12 < entries*8 {
		return nil, ErrParse(d)
	}

	d = d[12:]
	for len(d) > 0 {
		var key, val int //, flags int

		switch s.Version {
		case 2:
			//flags = int(d[3])
			key = int(fromLittle32(d[0:]) & 0xFFFFFF)
			val = int(fromBig32(d[4:]))
		case 3:
			//flags = int(d[0])
			key = int(fromBig32(d[0:]) & 0xFFFFFF)
			val = int(fromBig32(d[4:]))
		default:
			return nil, ErrSessionVersion(s.Version)
		}

		d = d[8:]

		if key == windowSetting {
			s.HaveWindow = true
			s.Window = val
			if s.Version == 2 {
				s.Window *= 1024
			}
			if s.Window < 0 {
				return nil, ErrSessionProtocol
			}
		}
	}

	return s, nil
}

type pingFrame struct {
	Version int
	Id      uint32
}

func (s *pingFrame) WriteFrame(w io.Writer, c *compressor) error {
	Log("spdy: tx PING %+v", s)
	h := [12]byte{}
	toBig32(h[0:], pingCode|uint32(s.Version<<16))
	toBig32(h[4:], 4) // length 4 and no flags
	toBig32(h[8:], s.Id)
	_, err := w.Write(h[:])
	return err
}

func parsePing(d []byte) (*pingFrame, error) {
	if len(d) < 12 {
		return nil, ErrParse(d)
	}

	return &pingFrame{
		Version: int(fromBig16(d) & 0x7FFF),
		Id:      fromBig32(d[8:]),
	}, nil
}

type goAwayFrame struct {
	Version      int
	LastStreamId int
	Reason       int
}

func (s *goAwayFrame) WriteFrame(w io.Writer, c *compressor) (err error) {
	Log("spdy: tx GO_AWAY %+v", s)
	h := [16]byte{}
	toBig32(h[0:], goAwayCode|uint32(s.Version<<16))
	toBig32(h[4:], 8) // length 8 and no flags
	toBig32(h[8:], uint32(s.LastStreamId))
	toBig32(h[12:], uint32(s.Reason))

	switch s.Version {
	case 2:
		// no reason
		_, err = w.Write(h[:12])
	case 3:
		_, err = w.Write(h[:])
	default:
		err = ErrSessionVersion(s.Version)
	}

	return err
}

func parseGoAway(d []byte) (*goAwayFrame, error) {
	if len(d) < 12 {
		return nil, ErrParse(d)
	}

	s := &goAwayFrame{
		Version:      int(fromBig16(d) & 0x7FFF),
		LastStreamId: int(fromBig32(d[8:])),
		Reason:       rstSuccess,
	}

	if s.Version >= 3 {
		if len(d) < 16 {
			return nil, ErrParse(d)
		}

		s.Reason = int(fromBig32(d[12:]))
	}

	if s.LastStreamId < 0 {
		return nil, ErrSessionProtocol
	}

	return s, nil
}

type dataFrame struct {
	StreamId int
	Finished bool
	Data     []byte
}

func (s *dataFrame) WriteFrame(w io.Writer, c *compressor) error {
	Log("spdy: tx DATA %+v %s", s, s.Data)

	flags := uint32(0)
	if s.Finished {
		flags |= finishedFlag << 24
	}

	h := [8]byte{}
	toBig32(h[0:], uint32(s.StreamId))
	toBig32(h[4:], flags|uint32(len(s.Data)))

	if _, err := w.Write(h[:]); err != nil {
		return err
	}

	if _, err := w.Write(s.Data); err != nil {
		return err
	}

	return nil
}

func parseData(d []byte) (*dataFrame, error) {
	s := &dataFrame{
		StreamId: int(fromBig32(d[0:])),
		Finished: (d[4] & finishedFlag) != 0,
		Data:     d[8:],
	}

	if s.StreamId < 0 {
		return nil, ErrStreamProtocol(s.StreamId)
	}

	return s, nil
}
