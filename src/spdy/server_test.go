package spdy

import (
	"testing"
)

func TestListenAndServe(t *testing.T) {
	if err := ListenAndServe(":8080", nil); err != nil {
		t.Error(err)
	}
}

func TestListenAndServeTLS(t *testing.T) {
	if err := ListenAndServeTLS(":443", "foobar.co.nz.crt", "foobar.co.nz.key", nil); err != nil {
		t.Error(err)
	}
}
