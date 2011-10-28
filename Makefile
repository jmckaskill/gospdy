.PHONY: all


all:
	gofmt -w src/spdy/*.go src/server/*.go
	govet src/spdy/*.go src/server/*.go
	GOPATH="`pwd`" goinstall -nuke server
	GOPATH="`pwd`" goinstall -nuke client
