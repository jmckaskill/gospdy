package spdy

import (
	"time"
	"sync"
	"io"
	"http"
	"os"
)

type writeFlusher interface {
	io.Writer
	http.Flusher
}

type maxLatencyWriter struct {
	dst     writeFlusher
	latency int64 // nanos

	lk   sync.Mutex // protects init of done, as well Write + Flush
	done chan bool
}

func MaxLatencyWriter(w io.Writer, latency int64) io.Writer {
	if latency <= 0 {
		return w
	}

	dst, ok := w.(writeFlusher)
	if !ok {
		return w
	}

	return &maxLatencyWriter{
		dst:     dst,
		latency: latency,
	}
}

func (m *maxLatencyWriter) flushLoop() {
	t := time.NewTicker(m.latency)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			m.lk.Lock()
			m.dst.Flush()
			m.lk.Unlock()
		case <-m.done:
			return
		}
	}
	panic("unreached")
}

func (m *maxLatencyWriter) Write(p []byte) (n int, err os.Error) {
	m.lk.Lock()
	defer m.lk.Unlock()
	if m.done == nil {
		m.done = make(chan bool)
		go m.flushLoop()
	}
	n, err = m.dst.Write(p)
	if err != nil {
		m.done <- true
	}
	return
}

type maxLatencyReader struct {
	w   maxLatencyWriter
	src io.Reader
}

func MaxLatencyReader(r io.Reader, latency int64) io.Reader {
	if latency <= 0 {
		return r
	}

	l := &maxLatencyReader{}
	l.src = r
	l.w.latency = latency
	return l
}

func (m *maxLatencyReader) Read(b []byte) (n int, err os.Error) {
	return m.src.Read(b)
}

func (m *maxLatencyReader) WriteTo(w io.Writer) (n int64, err os.Error) {
	if dst, ok := w.(writeFlusher); ok {
		m.w.dst = dst
		return io.Copy(&m.w, m.src)
	}

	return io.Copy(w, m.src)
}
