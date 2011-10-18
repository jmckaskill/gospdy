.PHONY: all

all:
	gofmt -w src
	govet src
	GOPATH="`pwd`" goinstall -nuke spdy
