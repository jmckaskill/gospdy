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
	ErrGoAway             = errors.New("spdy: go away")
	ErrSessionFlowControl = errors.New("spdy: flow control error")
	ErrSessionProtocol    = errors.New("sydy: protocol error")
	ErrWriteAfterClose    = errors.New("spdy: write to closed stream")
)

type ErrStreamProtocol int
type ErrInvalidStream int
type ErrRefusedStream int
type ErrCancel int
type ErrStreamFlowControl int
type ErrStreamInUse int
type ErrStreamAlreadyClosed int
type ErrSessionVersion int
type ErrParse []byte
type ErrUnsupportedProxy string
type ErrStreamVersion struct {
	streamId int
	version  int
}
type ErrInvalidAssociatedStream struct {
	streamId           int
	associatedStreamId int
}

func (s ErrParse) Error() string {
	data := []byte(s)
	if len(data) > 20 {
		return fmt.Sprintf("spdy: error parsing %X...", data[:20])
	}

	return fmt.Sprintf("spdy: error parsing %X", data)
}

func (s ErrUnsupportedProxy) Error() string {
	return fmt.Sprintf("spdy: unsupported proxy %s", string(s))
}

func (s ErrSessionVersion) resetCode() int { return rstUnsupportedVersion }
func (s ErrSessionVersion) Error() string {
	return fmt.Sprintf("spdy: unsupported version %d", int(s))
}

func (s ErrStreamVersion) StreamId() int  { return s.streamId }
func (s ErrStreamVersion) resetCode() int { return rstUnsupportedVersion }
func (s ErrStreamVersion) Error() string {
	return fmt.Sprintf("spdy: unsupported version %d in stream %d", s.version, s.streamId)
}

func (s ErrStreamProtocol) StreamId() int  { return int(s) }
func (s ErrStreamProtocol) resetCode() int { return rstProtocolError }
func (s ErrStreamProtocol) Error() string {
	return fmt.Sprintf("sydy: protocol error with stream %d", int(s))
}

func (s ErrInvalidStream) StreamId() int  { return int(s) }
func (s ErrInvalidStream) resetCode() int { return rstInvalidStream }
func (s ErrInvalidStream) Error() string {
	return fmt.Sprintf("spdy: stream %d does not exist", int(s))
}

func (s ErrInvalidAssociatedStream) StreamId() int  { return s.streamId }
func (s ErrInvalidAssociatedStream) resetCode() int { return rstInvalidStream }
func (s ErrInvalidAssociatedStream) Error() string {
	return fmt.Sprintf("spdy: associated stream %d does not exist whilst creating stream %d", s.associatedStreamId, s.streamId)
}

func (s ErrRefusedStream) StreamId() int  { return int(s) }
func (s ErrRefusedStream) resetCode() int { return rstRefusedStream }
func (s ErrRefusedStream) Error() string {
	return fmt.Sprintf("spdy: stream %d refused", int(s))
}

func (s ErrCancel) StreamId() int  { return int(s) }
func (s ErrCancel) resetCode() int { return rstCancel }
func (s ErrCancel) Error() string {
	return fmt.Sprintf("spdy: stream %d has been cancelled", int(s))
}

func (s ErrStreamFlowControl) StreamId() int  { return int(s) }
func (s ErrStreamFlowControl) resetCode() int { return rstFlowControlError }
func (s ErrStreamFlowControl) Error() string {
	return fmt.Sprintf("spdy: flow control error with stream %d", int(s))
}

func (s ErrStreamInUse) StreamId() int  { return int(s) }
func (s ErrStreamInUse) resetCode() int { return rstStreamInUse }
func (s ErrStreamInUse) Error() string {
	return fmt.Sprintf("spdy: stream id %d was already being used", int(s))
}

func (s ErrStreamAlreadyClosed) StreamId() int  { return int(s) }
func (s ErrStreamAlreadyClosed) resetCode() int { return rstStreamAlreadyClosed }
func (s ErrStreamAlreadyClosed) Error() string {
	return fmt.Sprintf("spdy: stream %d has already been closed", int(s))
}
