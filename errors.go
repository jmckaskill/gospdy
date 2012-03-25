package spdy

import (
	"errors"
	"fmt"
)

type sessionError interface {
	resetCode() int
}

type streamError interface {
	StreamId() int
	resetCode() int
}

var (
	errGoAway             = errors.New("spdy: go away")
	errSessionFlowControl = errors.New("spdy: flow control error")
	errSessionProtocol    = errors.New("sydy: protocol error")
	errWriteAfterClose    = errors.New("spdy: write to closed stream")
	errReadAfterClose     = errors.New("spdy: read to closed stream")
)

type errStreamProtocol int
type errInvalidStream int
type errRefusedStream int
type errCancel int
type errStreamFlowControl int
type errStreamInUse int
type errStreamAlreadyClosed int
type errSessionVersion int
type errParse []byte
type errUnsupportedProxy string
type errStreamVersion struct {
	streamId int
	version  int
}
type errInvalidAssociatedStream struct {
	streamId           int
	associatedStreamId int
}

func (s errParse) Error() string {
	data := []byte(s)
	if len(data) > 20 {
		return fmt.Sprintf("spdy: error parsing %X...", data[:20])
	}

	return fmt.Sprintf("spdy: error parsing %X", data)
}

func (s errUnsupportedProxy) Error() string {
	return fmt.Sprintf("spdy: unsupported proxy %s", string(s))
}

func (s errSessionVersion) resetCode() int { return rstUnsupportedVersion }
func (s errSessionVersion) Error() string {
	return fmt.Sprintf("spdy: unsupported version %d", int(s))
}

func (s errStreamVersion) StreamId() int  { return s.streamId }
func (s errStreamVersion) resetCode() int { return rstUnsupportedVersion }
func (s errStreamVersion) Error() string {
	return fmt.Sprintf("spdy: unsupported version %d in stream %d", s.version, s.streamId)
}

func (s errStreamProtocol) StreamId() int  { return int(s) }
func (s errStreamProtocol) resetCode() int { return rstProtocolError }
func (s errStreamProtocol) Error() string {
	return fmt.Sprintf("sydy: protocol error with stream %d", int(s))
}

func (s errInvalidStream) StreamId() int  { return int(s) }
func (s errInvalidStream) resetCode() int { return rstInvalidStream }
func (s errInvalidStream) Error() string {
	return fmt.Sprintf("spdy: stream %d does not exist", int(s))
}

func (s errInvalidAssociatedStream) StreamId() int  { return s.streamId }
func (s errInvalidAssociatedStream) resetCode() int { return rstInvalidStream }
func (s errInvalidAssociatedStream) Error() string {
	return fmt.Sprintf("spdy: associated stream %d does not exist whilst creating stream %d", s.associatedStreamId, s.streamId)
}

func (s errRefusedStream) StreamId() int  { return int(s) }
func (s errRefusedStream) resetCode() int { return rstRefusedStream }
func (s errRefusedStream) Error() string {
	return fmt.Sprintf("spdy: stream %d refused", int(s))
}

func (s errCancel) StreamId() int  { return int(s) }
func (s errCancel) resetCode() int { return rstCancel }
func (s errCancel) Error() string {
	return fmt.Sprintf("spdy: stream %d has been cancelled", int(s))
}

func (s errStreamFlowControl) StreamId() int  { return int(s) }
func (s errStreamFlowControl) resetCode() int { return rstFlowControlError }
func (s errStreamFlowControl) Error() string {
	return fmt.Sprintf("spdy: flow control error with stream %d", int(s))
}

func (s errStreamInUse) StreamId() int  { return int(s) }
func (s errStreamInUse) resetCode() int { return rstStreamInUse }
func (s errStreamInUse) Error() string {
	return fmt.Sprintf("spdy: stream id %d was already being used", int(s))
}

func (s errStreamAlreadyClosed) StreamId() int  { return int(s) }
func (s errStreamAlreadyClosed) resetCode() int { return rstStreamAlreadyClosed }
func (s errStreamAlreadyClosed) Error() string {
	return fmt.Sprintf("spdy: stream %d has already been closed", int(s))
}
