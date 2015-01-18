// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package circular

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/maruel/ut"
)

func TestBufferInternalState(t *testing.T) {
	data := []struct {
		write       int
		rolled      bool
		writtenSize int
		written     string
		buf         string
	}{
		// 0 Nil write
		{0, false, -1, "", "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"},
		// 1 Normal write.
		{5, false, -1, "Hello", "Hello\x00\x00\x00\x00\x00"},
		// 2 Rolling over.
		{3, true, -1, "Crossing", "ingloCross"},
		// 3 Ending at buffer end.
		{0, true, -1, "3456789", "ing3456789"},
		// 4 Data longer than buffer, ending at buffer end.
		{9, true, 9, "Overly long string", "ng string9"},
	}
	b := MakeBuffer(10)
	for i, item := range data {
		n, err := b.Write([]byte(item.written))
		if item.writtenSize == -1 {
			ut.AssertEqualIndex(t, i, len(item.written), n)
		} else {
			ut.AssertEqualIndex(t, i, item.writtenSize, n)
		}
		ut.AssertEqualIndex(t, i, nil, err)
		ut.AssertEqualIndex(t, i, item.buf, string(b.buf))
		ut.AssertEqualIndex(t, i, item.rolled, b.rolledOver)
		ut.AssertEqualIndex(t, i, item.write, b.writeOffset)
	}
}

func TestBufferNoHangOnFail(t *testing.T) {
	b := MakeBuffer(10)
	writeOk(t, b, "hello")
	actualErr := errors.New("Failed")
	// Returns immediately.
	n, err := b.WriteTo(&failWriter{actualErr})
	ut.AssertEqual(t, 0, n)
	ut.AssertEqual(t, actualErr, err)
}

func TestBufferZero(t *testing.T) {
	ut.AssertEqual(t, (*Buffer)(nil), MakeBuffer(1))
}

func TestBufferDidntUseMakeBuffer(t *testing.T) {
	// Incorrect.
	b := &Buffer{}
	n, err := b.Write([]byte("foo"))
	ut.AssertEqual(t, 0, n)
	ut.AssertEqual(t, io.ErrClosedPipe, err)
}

func TestBufferRolledOver(t *testing.T) {
	b := MakeBuffer(10)
	writeOk(t, b, "HelloHell")
	writeOk(t, b, "o")
	ut.AssertEqual(t, true, b.rolledOver)
}

func TestBufferEOF(t *testing.T) {
	b := MakeBuffer(10)
	writeOk(t, b, "Hello")
	actualErr := errors.New("Failed")
	n, err := b.WriteTo(&failWriter{actualErr})
	ut.AssertEqual(t, 0, n)
	ut.AssertEqual(t, actualErr, err)
}

func TestBufferClosed(t *testing.T) {
	b := MakeBuffer(10)
	ut.AssertEqual(t, nil, b.Close())
	n, err := b.Write([]byte("foo"))
	ut.AssertEqual(t, 0, n)
	ut.AssertEqual(t, io.ErrClosedPipe, err)
	ut.AssertEqual(t, io.ErrClosedPipe, b.Close())
}

func TestBufferReaders(t *testing.T) {
	b := MakeBuffer(10)
	buffers := [2]bytes.Buffer{}
	var wgEnd sync.WaitGroup
	var wgReady sync.WaitGroup
	wgReady.Add(2)

	for i := 0; i < len(buffers); i++ {
		wgEnd.Add(1)
		go func(j int) {
			defer wgEnd.Done()
			wgReady.Done()
			n, err := b.WriteTo(&buffers[j])
			ut.AssertEqual(t, 9, n)
			ut.AssertEqual(t, io.EOF, err)
		}(i)
	}

	wgReady.Wait()
	writeOk(t, b, "Hello")
	writeOk(t, b, "Hallo")
	// This one causes dataloss.
	writeLimited(t, b, "Overflowed totally", 9)

	err := b.Close()
	ut.AssertEqual(t, nil, err)
	wgEnd.Wait()

	for i := 0; i < len(buffers); i++ {
		// The readers skipped and missed data. See BUG in circular.go.
		ut.AssertEqual(t, "d totally", buffers[i].String())
	}
}

func TestBufferFlusher(t *testing.T) {
	b := MakeBuffer(10)
	w := &flusherWriter{}
	var wgEnd sync.WaitGroup
	wgEnd.Add(1)
	var wgReady sync.WaitGroup
	wgReady.Add(1)
	ut.AssertEqual(t, false, w.flushed)

	go func() {
		defer wgEnd.Done()
		wgReady.Done()
		n, err := b.WriteTo(w)
		ut.AssertEqual(t, 7, n)
		ut.AssertEqual(t, io.EOF, err)
		ut.AssertEqual(t, "HelloHi", w.buf.String())
	}()

	wgReady.Wait()
	writeOk(t, b, "Hello")
	writeOk(t, b, "Hi")
	ut.AssertEqual(t, nil, b.Close())
	wgEnd.Wait()
	ut.AssertEqual(t, true, w.flushed)
}

func TestBufferFlusherRolledOver(t *testing.T) {
	b := MakeBuffer(10)
	writeOk(t, b, "hello")
	writeOk(t, b, "HELLO")
	w := &flusherWriter{}
	var wgEnd sync.WaitGroup
	wgEnd.Add(1)
	var wgReady sync.WaitGroup
	wgReady.Add(1)

	go func() {
		defer wgEnd.Done()
		wgReady.Done()
		n, err := b.WriteTo(w)
		ut.AssertEqual(t, 19, n)
		ut.AssertEqual(t, io.EOF, err)
		ut.AssertEqual(t, "helloHELLO nice day", w.buf.String())
	}()

	wgReady.Wait()
	writeLimited(t, b, "Have a nice day", 9)
	ut.AssertEqual(t, nil, b.Close())
	wgEnd.Wait()
}

// Utilities.

type failWriter struct {
	err error
}

func (f *failWriter) Write(p []byte) (int, error) {
	return 0, f.err
}

func writeOk(t *testing.T, w io.Writer, s string) {
	n, err := w.Write([]byte(s))
	ut.AssertEqual(t, len(s), n)
	ut.AssertEqual(t, nil, err)
}

func writeLimited(t *testing.T, w io.Writer, s string, expected int) {
	n, err := w.Write([]byte(s))
	ut.AssertEqual(t, expected, n)
	ut.AssertEqual(t, nil, err)
}

type flusherWriter struct {
	buf     bytes.Buffer
	flushed bool
}

func (f *flusherWriter) Write(p []byte) (int, error) {
	return f.buf.Write(p)
}

// Flush implements http.Flusher.
func (f *flusherWriter) Flush() {
	f.flushed = true
}
