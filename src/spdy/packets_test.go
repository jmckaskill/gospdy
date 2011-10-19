package spdy

import (
	"bytes"
	"os"
	"testing"
	"url"
)

func TestSynStreamFrame(t *testing.T) {
	var err os.Error
	buf := &bytes.Buffer{}
	zip := &compressor{}
	unzip := &decompressor{}

	f := &synStreamFrame{
		Version:            2,
		Finished:           true,
		Unidirectional:     false,
		StreamId:           2,
		AssociatedStreamId: 0,
		Header:             nil,
		Priority:           4,
		Proto:              "https",
		Method:             "GET",
	}

	if f.URL, err = url.Parse("https://www.google.com/index.html"); err != nil {
		t.Fatal(err)
	}

	if err = f.WriteTo(buf, zip); err != nil && err != os.EOF {
		t.Fatal("synStreamFrame.WriteTo", err)
	}

	t.Logf("SYN_STREAM %x", buf.Bytes())

	f2, err := parseSynStream(buf.Bytes(), unzip)
	if err != nil {
		t.Fatal("parseSynStream", err)
	}

	if f.Version != f2.Version {
		t.Fatal("version mismatch")
	}

	if f.Finished != f2.Finished {
		t.Fatal("finished mismatch")
	}

	if f.Unidirectional != f2.Unidirectional {
		t.Fatal("unidirectional mismatch")
	}

	if f.StreamId != f2.StreamId {
		t.Fatal("stream id mismatch")
	}

	if f.AssociatedStreamId != f2.AssociatedStreamId {
		t.Fatal("associated stream id mismatch")
	}

	if f.Priority != f2.Priority {
		t.Fatal("priority mismatch")
	}

	if f.URL.String() != f2.URL.String() {
		t.Fatal("url mismatch")
	}

	if f.Proto != f2.Proto {
		t.Fatal("proto mismatch")
	}

	if f.Method != f2.Method {
		t.Fatal("method mismatch")
	}
}
