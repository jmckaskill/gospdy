package spdy

import (
	"bytes"
	"fmt"
	"http"
	"reflect"
	"testing"
	"url"
)

func equal2(f, t reflect.Value) bool {
	if f.Type() != t.Type() {
		panic("different types")
	}

	switch f.Kind() {
	case reflect.Array, reflect.Slice:
		if f.Len() != t.Len() {
			return false
		}

		for i := 0; i < f.Len(); i++ {
			if !equal2(f.Index(i), t.Index(i)) {
				return false
			}
		}

	case reflect.String:
		if f.String() != t.String() {
			return false
		}

	case reflect.Struct:
		for i := 0; i < f.NumField(); i++ {
			if !equal2(t.Field(i), f.Field(i)) {
				return false
			}
		}

	case reflect.Map:
		if f.Len() != t.Len() {
			return false
		}

		for _, k := range f.MapKeys() {
			if !equal2(f.MapIndex(k), t.MapIndex(k)) {
				return false
			}
		}

	case reflect.Bool:
		if f.Bool() != t.Bool() {
			return false
		}

	case reflect.Int:
		if f.Int() != t.Int() {
			return false
		}

	case reflect.Uint32:
		if f.Uint() != t.Uint() {
			return false
		}

	case reflect.Interface, reflect.Ptr:
		if equal2(f.Elem(), t.Elem()) {
			return false
		}

	default:
		panic(fmt.Sprintf("not implemented %s", f.Kind()))
	}

	return true
}

func equal(f, t interface{}) bool {
	return equal2(reflect.ValueOf(f), reflect.ValueOf(t))
}

func TestSynStreamFrame(t *testing.T) {
	buf := &bytes.Buffer{}
	zip := &compressor{}
	unzip := &decompressor{}

	f := synStreamFrame{
		Version:            2,
		Finished:           true,
		Unidirectional:     false,
		StreamId:           2,
		AssociatedStreamId: 0,
		Header:             nil,
		Priority:           HighPriority,
		Proto:              "HTTP/1.1",
		Method:             "GET",
		ProtoMajor:         1,
		ProtoMinor:         1,
	}

	f.URL, _ = url.Parse("https://www.google.com/index.html")

	test := func() {
		buf.Reset()
		if err := f.WriteTo(buf, zip); err != nil {
			t.Fatalf("%v %+v", err, f)
		}

		if f2, err := parseSynStream(buf.Bytes(), unzip); err != nil {
			t.Fatalf("%v %+v", err, f)
		} else if !equal(f, f2) {
			t.Fatalf("%+v\n%+v", f, f2)
		}
	}

	f.Version = 2
	test()
	f.Version = 3
	test()

	f.Header = make(http.Header)

	f.Version = 2
	test()
	f.Version = 3
	test()

	f.Header.Add("WWW-Authentication", "foobar")

	f.Version = 2
	test()
	f.Version = 3
	test()
}
