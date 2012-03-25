package spdy

import "io"

// buffer is a fixed size buffer that is used for receiving data on the
// session rx thread.
type buffer struct {
	begin int
	end   int
	buf   [defaultBufferSize * 2]byte
}

func (s *buffer) Get(r io.Reader, n int) ([]byte, error) {
	// compress the data we already have
	if s.begin > defaultBufferSize {
		copy(s.buf[:], s.buf[s.begin:s.end])
		s.end -= s.begin
		s.begin = 0
	}

	toget := len(s.buf) - s.end
	if toget > defaultBufferSize {
		toget = defaultBufferSize
	}

	// only get up to the buffer size
	if n > defaultBufferSize {
		n = defaultBufferSize
	}

	for s.end-s.begin < n {
		got, err := r.Read(s.buf[s.end : s.end+toget])
		s.end += got
		if err != nil {
			return nil, err
		}
	}

	return s.buf[s.begin : s.begin+n], nil
}

func (s *buffer) Flush(n int) {
	s.begin += n
}
