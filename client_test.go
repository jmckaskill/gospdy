package spdy

import (
	"io"
	"io/ioutil"
	"net/http"
	"testing"
)

func getGoogle(t *testing.T, c *http.Client) {
	r, err := c.Get("https://www.google.com")
	if err != nil {
		t.Log(err)
		return
	}
	defer r.Body.Close()

	if _, err := io.Copy(ioutil.Discard, r.Body); err != nil {
		t.Log(err)
		return
	}
}

func TestTransport(t *testing.T) {
	tr := &Transport{}
	c := &http.Client{Transport: tr}

	Log = func(format string, args ...interface{}) {
		t.Logf(format, args...)
	}

	getGoogle(t, c)

	tr.Unidirectional = true
	getGoogle(t, c)
}
