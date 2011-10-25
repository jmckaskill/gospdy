include $(GOROOT)/src/Make.inc

TARG=spdy
GOFILES=\
	buffer.go \
	client.go \
	connection.go \
	errors.go \
	latency.go \
	logging.go \
	packets.go \
	server.go \
	stream.go

include $(GOROOT)/src/Make.pkg
