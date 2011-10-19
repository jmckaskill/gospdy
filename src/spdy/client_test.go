package spdy

import (
	"io"
	"os"
	"testing"
)

func TestNewClient(t *testing.T) {
	r, err := DefaultClient.Get("https://encrypted.google.com/")
	if err != nil {
		t.Error(err)
		return
	}

	t.Log(r)
	io.Copy(os.Stdout, r.Body)
}
