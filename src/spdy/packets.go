package spdy

import (
	"bytes"
	"compress/zlib"
	"http"
	"io"
	"os"
	"strings"
)

const (
	Version         = 3
	MaxPriority     = 8
	DefaultPriority = 4

	synStreamControlType = 1

	synStreamCode    = (1 << 31) | (Version << 16) | 1
	synReplyCode     = (1 << 31) | (Version << 16) | 2
	rstStreamCode    = (1 << 31) | (Version << 16) | 3
	settingsCode     = (1 << 31) | (Version << 16) | 4
	pingCode         = (1 << 31) | (Version << 16) | 6
	goAwayCode       = (1 << 31) | (Version << 16) | 7
	headersCode      = (1 << 31) | (Version << 16) | 8
	windowUpdateCode = (1 << 31) | (Version << 16) | 9

	finishedFlag       = 1
	compressFlag       = 2
	unidirectionalFlag = 2

	windowSetting = 5

	headerDictionary = `optionsgetheadpostputdeletetraceacceptaccept-charsetaccept-encodingaccept-languageauthorizationexpectfromhostif-modified-sinceif-matchif-none-matchif-rangeif-unmodifiedsincemax-forwardsproxy-authorizationrangerefererteuser-agent100101200201202203204205206300301302303304305306307400401402403404405406407408409410411412413414415416417500501502503504505accept-rangesageetaglocationproxy-authenticatepublicretry-afterservervarywarningwww-authenticateallowcontent-basecontent-encodingcache-controlconnectiondatetrailertransfer-encodingupgradeviawarningcontent-languagecontent-lengthcontent-locationcontent-md5content-rangecontent-typeetagexpireslast-modifiedset-cookieMondayTuesdayWednesdayThursdayFridaySaturdaySundayJanFebMarAprMayJunJulAugSepOctNovDecchunkedtext/htmlimage/pngimage/jpgimage/gifapplication/xmlapplication/xhtmltext/plainpublicmax-agecharset=iso-8859-1utf-8gzipdeflateHTTP/1.1statusversionurl` + "\x00"
	compressionLevel = zlib.DefaultCompression

	rstProtocolError       = 1
	rstInvalidStream       = 2
	rstRefusedStream       = 3
	rstUnsupportedVersion  = 4
	rstCancel              = 5
	rstFlowControlError    = 6
	rstStreamInUse         = 7
	rstStreamAlreadyClosed = 8
)

var parseError = os.NewError("parse error")

func toBig32(d []byte, val uint32) {
	d[0] = byte(val >> 24)
	d[1] = byte(val >> 16)
	d[2] = byte(val >> 8)
	d[3] = byte(val)
}

func fromBig32(d []byte) uint32 {
	return uint32(d[0])<<24 |
		uint32(d[1])<<16 |
		uint32(d[2])<<8 |
		uint32(d[3])
}

type decompressor struct {
	data []byte
	buf  *bytes.Buffer
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
		s.buf = bytes.NewBuffer(nil)

		var err os.Error
		s.zlib, err = zlib.NewReaderDict(s, []byte(headerDictionary))
		if err != nil {
			return nil, err
		}
	} else {
		s.buf.Reset()
	}

	s.data = data
	if _, err := io.Copy(s.buf, s.zlib); err != nil {
		return nil, err
	}

	h := s.buf.Bytes()
	headers := make(http.Header)
	for {
		if len(h) < 4 {
			return nil, parseError
		}

		klen := int(fromBig32(h))
		h = h[4:]

		if klen < 0 || len(h) < klen+4 {
			return nil, parseError
		}

		key := string(h[:klen])
		vlen := int(fromBig32(h))
		h = h[4:]

		if vlen < 0 || len(h) < vlen {
			return nil, parseError
		}

		val := h[:vlen]
		h = h[vlen:]
		vals := make([]string, 0)

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
	buf  *bytes.Buffer
	zlib *zlib.Writer
}

func (s *compressor) Begin(init []byte) {
	if s.buf == nil {
		s.buf = bytes.NewBuffer(make([]byte, 0, len(init)))

		var err os.Error
		s.zlib, err = zlib.NewWriterDict(s.buf, compressionLevel, []byte(headerDictionary))
		if err != nil {
			panic(err)
		}
	} else {
		s.buf.Reset()
	}

	s.buf.Write(init)
}

func (s *compressor) Compress(key string, val string) {
	var k, v [4]byte
	toBig32(k[:], uint32(len(key)))
	toBig32(v[:], uint32(len(val)))
	s.zlib.Write(k[:])
	s.zlib.Write([]byte(key))
	s.zlib.Write(v[:])
	s.zlib.Write([]byte(val))
}

func (s *compressor) CompressMulti(headers http.Header) {
	for key, val := range headers {
		if len(key) > 0 && key[0] != ':' {
			var k, v [4]byte

			toBig32(k[:], uint32(len(key)))
			s.zlib.Write(k[:])
			s.zlib.Write(bytes.ToLower([]byte(key)))

			vals := strings.Join(val, "\x00")
			toBig32(v[:], uint32(len(vals)))
			s.zlib.Write(v[:])
			s.zlib.Write([]byte(vals))
		}
	}
}

func (s *compressor) Finish() []byte {
	s.zlib.Flush()
	return s.buf.Bytes()
}

type frame interface {
	WriteTo(w io.Writer, c *compressor) os.Error
}

type synStreamFrame struct {
	Finished           bool
	Unidirectional     bool
	StreamId           int
	AssociatedStreamId int
	Header             http.Header
	Priority           int
	URL                *http.URL
	Proto              string
	Method             string
}

func (s *synStreamFrame) WriteTo(w io.Writer, c *compressor) os.Error {
	flags := uint32(0)
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
	toBig32(h[4:], flags|uint32(len(h)-8))
	toBig32(h[8:], uint32(s.StreamId))
	toBig32(h[12:], uint32(s.AssociatedStreamId))
	h[16] = byte(s.Priority << 5)
	h[17] = 0 // unused

	_, err := w.Write(h)
	return err
}

func parseSynStream(d []byte, c *decompressor) (s synStreamFrame, err os.Error) {
	if len(d) < 18 {
		return s, parseError
	}
	s.Finished = (d[4] & finishedFlag) != 0
	s.Unidirectional = (d[4] & unidirectionalFlag) != 0
	s.StreamId = int(fromBig32(d[8:]))
	s.AssociatedStreamId = int(fromBig32(d[12:]))
	s.Priority = int(d[16] >> 5)

	if s.StreamId <= 0 || s.AssociatedStreamId < 0 {
		return s, parseError
	}

	if s.Header, err = c.Decompress(d[18:]); err != nil {
		return s, err
	}

	s.Proto = s.Header.Get(":version")
	s.Method = s.Header.Get(":method")
	u := &http.URL{
		Scheme: s.Header.Get(":scheme"),
		Host:   s.Header.Get(":host"),
		Path:   s.Header.Get(":path"),
	}

	if q := strings.Index(u.Path, "?"); q > 0 {
		u.RawQuery = u.Path[q+1:]
		u.Path = u.Path[:q]
	}

	if len(s.Proto) == 0 ||
		len(s.Method) == 0 ||
		len(u.Scheme) == 0 ||
		len(u.Host) == 0 ||
		len(u.Path) == 0 ||
		u.Path[0] != '/' {

		return s, parseError
	}

	// TODO(james): error on not allowed headers (eg Connection)

	u.Raw = u.String()
	s.URL = u

	return s, err
}

type synReplyFrame struct {
	Finished bool
	StreamId int
	Header   http.Header
	Status   string
	Proto    string
}

func (s *synReplyFrame) WriteTo(w io.Writer, c *compressor) os.Error {
	flags := uint32(0)
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
	toBig32(h[4:], flags|uint32(len(h)-8))
	toBig32(h[8:], uint32(s.StreamId))

	_, err := w.Write(h)
	return err
}

func parseSynReply(d []byte, c *decompressor) (s synReplyFrame, err os.Error) {
	if len(d) < 12 {
		return s, parseError
	}
	s.Finished = (d[4] & finishedFlag) != 0
	s.StreamId = int(fromBig32(d[8:]))
	if s.StreamId < 0 {
		return s, parseError
	}
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
	Header   http.Header
}

func (s *headersFrame) WriteTo(w io.Writer, c *compressor) os.Error {
	flags := uint32(0)
	if s.Finished {
		flags |= finishedFlag << 24
	}

	c.Begin([12]byte{}[:])
	c.CompressMulti(s.Header)
	h := c.Finish()
	toBig32(h[0:], headersCode)
	toBig32(h[4:], flags|uint32(len(h)-8))
	toBig32(h[8:], uint32(s.StreamId))

	_, err := w.Write(h)
	return err
}

func parseHeaders(d []byte, c *decompressor) (s headersFrame, err os.Error) {
	if len(d) < 12 {
		return s, parseError
	}

	s.Finished = (d[4] & finishedFlag) != 0
	s.StreamId = int(fromBig32(d[8:]))
	if s.StreamId < 0 {
		return s, parseError
	}
	if s.Header, err = c.Decompress(d[12:]); err != nil {
		return s, err
	}

	return s, nil
}

type rstStreamFrame struct {
	StreamId int
	Reason   int
}

func (s rstStreamFrame) WriteTo(w io.Writer, c *compressor) os.Error {
	h := [16]byte{}
	toBig32(h[0:], rstStreamCode)
	toBig32(h[4:], 8) // length and no flags
	toBig32(h[8:], uint32(s.StreamId))
	toBig32(h[12:], uint32(s.Reason))
	_, err := w.Write(h[:])
	return err
}

func parseRstStream(d []byte) (s rstStreamFrame, err os.Error) {
	if len(d) != 16 {
		return s, parseError
	}

	s.StreamId = int(fromBig32(d[8:]))
	s.Reason = int(fromBig32(d[12:]))

	if s.StreamId < 0 || s.Reason == 0 {
		return s, parseError
	}

	return s, nil
}

type windowUpdateFrame struct {
	StreamId    int
	WindowDelta int
}

func (s windowUpdateFrame) WriteTo(w io.Writer, c *compressor) os.Error {
	h := [16]byte{}
	toBig32(h[0:], windowUpdateCode)
	toBig32(h[4:], 8) // length and no flags
	toBig32(h[8:], uint32(s.StreamId))
	toBig32(h[12:], uint32(s.WindowDelta))
	_, err := w.Write(h[:])
	return err
}

func parseWindowUpdate(d []byte) (s windowUpdateFrame, err os.Error) {
	if len(d) != 16 {
		return s, parseError
	}

	s.StreamId = int(fromBig32(d[8:]))
	s.WindowDelta = int(fromBig32(d[12:]))

	if s.StreamId < 0 || s.WindowDelta <= 0 {
		return s, parseError
	}

	return s, nil
}

type settingsFrame struct {
	HaveWindow bool
	Window     int
}

func (s settingsFrame) WriteTo(w io.Writer, c *compressor) os.Error {
	if !s.HaveWindow {
		return nil
	}

	h := [20]byte{}
	toBig32(h[0:], settingsCode)
	toBig32(h[4:], 12) // length from here and no flags
	toBig32(h[8:], 1)  // number of entries
	toBig32(h[12:], windowSetting)
	toBig32(h[16:], uint32(s.Window))
	_, err := w.Write(h[:])
	return err
}

func parseSettings(d []byte) (s settingsFrame, err os.Error) {
	if len(d) < 12 {
		return s, parseError
	}

	entries := int(fromBig32(d[8:]))
	if entries < 0 || entries > 4096 {
		return s, parseError
	}

	if len(d)-12 != entries*8 {
		return s, parseError
	}

	d = d[12:]
	for len(d) > 0 {
		key := fromBig32(d)
		val := fromBig32(d[4:])
		d = d[8:]

		if key == windowSetting {
			s.HaveWindow = true
			s.Window = int(val)
			if s.Window < 0 {
				return s, parseError
			}
		}
	}

	return s, nil
}

type pingFrame struct {
	Id uint32
}

func (s pingFrame) WriteTo(w io.Writer, c *compressor) os.Error {
	h := [12]byte{}
	toBig32(h[0:], pingCode)
	toBig32(h[4:], 4) // length 4 and no flags
	toBig32(h[8:], s.Id)
	_, err := w.Write(h[:])
	return err
}

func parsePing(d []byte) (s pingFrame, err os.Error) {
	if len(d) != 12 {
		return s, parseError
	}
	return pingFrame{fromBig32(d[8:])}, nil
}

type goAwayFrame struct {
	LastStreamId int
	Reason       int
}

func (s goAwayFrame) WriteTo(w io.Writer, c *compressor) os.Error {
	h := [16]byte{}
	toBig32(h[0:], goAwayCode)
	toBig32(h[4:], 8) // length 8 and no flags
	toBig32(h[8:], uint32(s.LastStreamId))
	toBig32(h[12:], uint32(s.Reason))
	return nil
}

func parseGoAway(d []byte) (s goAwayFrame, err os.Error) {
	if len(d) != 16 {
		return s, parseError
	}

	s.LastStreamId = int(fromBig32(d[8:]))
	s.Reason = int(fromBig32(d[12:]))

	if s.LastStreamId < 0 {
		return s, parseError
	}

	return s, nil
}

type dataFrame struct {
	Data     []byte
	StreamId int
	Finished bool
}

func (s dataFrame) WriteTo(w io.Writer, c *compressor) os.Error {
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

func parseData(d []byte) (s dataFrame, err os.Error) {
	s.StreamId = int(fromBig32(d[0:]))
	s.Finished = (d[4] & finishedFlag) != 0
	s.Data = d[8:]
	if s.StreamId < 0 {
		return s, parseError
	}
	return s, nil
}
