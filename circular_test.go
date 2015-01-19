// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package circular

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/maruel/ut"
)

func TestBufferInternalState(t *testing.T) {
	data := []struct {
		written string
		buf     string
	}{
		// 0 Nil write
		{"", "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"},
		// 1 Normal write.
		{"Hello", "Hello\x00\x00\x00\x00\x00"},
		// 2 Rolling over.
		{"Crossing", "ingloCross"},
		// 3 Ending at buffer end.
		{"3456789", "ing3456789"},
		// 4 Data longer than buffer, ending at buffer end.
		{"Overly long string", "g stringon"},
	}
	bytesWritten := 0
	b := MakeBuffer(10)
	for i, item := range data {
		writeOk(t, b, item.written)
		bytesWritten += len(item.written)
		ut.AssertEqualIndex(t, i, item.buf, string(b.buf))
		ut.AssertEqualIndex(t, i, bytesWritten, b.bytesWritten)
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
	ut.AssertEqual(t, (*Buffer)(nil), MakeBuffer(0))
}

func TestBufferDidntUseMakeBuffer(t *testing.T) {
	// Incorrect.
	b := &Buffer{}
	n, err := b.Write([]byte("foo"))
	ut.AssertEqual(t, 0, n)
	ut.AssertEqual(t, io.ErrClosedPipe, err)
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

func TestBufferRolledOver(t *testing.T) {
	var end End
	b := MakeBuffer(10)
	buffer := &bytes.Buffer{}
	writeOk(t, b, "Overflowed totally")
	var wg sync.WaitGroup

	wg.Add(1)
	end.Go(func() {
		wg.Done()
		n, err := b.WriteTo(buffer)
		ut.AssertEqual(t, 13, n)
		ut.AssertEqual(t, io.EOF, err)
	})
	wg.Wait()
	writeOk(t, b, "hey")
	ut.AssertEqual(t, nil, b.Close())
	end.Wait()
	ut.AssertEqual(t, "ed totallyhey", buffer.String())
}

func TestBufferReaders(t *testing.T) {
	b := MakeBuffer(10)
	// TODO(maruel): Create state machine to ensure all possible race conditions
	// are addressed.
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
			ut.AssertEqual(t, 28, n)
			ut.AssertEqual(t, io.EOF, err)
		}(i)
	}

	wgReady.Wait()
	writeOk(t, b, "Hello")
	writeOk(t, b, "Hallo")
	writeOk(t, b, "Overflowed totally")

	err := b.Close()
	ut.AssertEqual(t, nil, err)
	wgEnd.Wait()

	for i := 0; i < len(buffers); i++ {
		ut.AssertEqual(t, "HelloHalloOverflowed totally", buffers[i].String())
	}
}

func TestBufferFlusher(t *testing.T) {
	var end End
	b := MakeBuffer(10)
	w := &flusherWriter{}
	var wgReady sync.WaitGroup
	wgReady.Add(1)
	ut.AssertEqual(t, false, w.flushed)

	end.Go(func() {
		wgReady.Done()
		n, err := b.WriteTo(w)
		ut.AssertEqual(t, 7, n)
		ut.AssertEqual(t, io.EOF, err)
		ut.AssertEqual(t, "HelloHi", w.buf.String())
	})

	wgReady.Wait()
	writeOk(t, b, "Hello")
	writeOk(t, b, "Hi")
	ut.AssertEqual(t, nil, b.Close())
	end.Wait()
	ut.AssertEqual(t, true, w.flushed)
}

func TestBufferFlusherRolledOver(t *testing.T) {
	var end End
	b := MakeBuffer(10)
	writeOk(t, b, "hello")
	writeOk(t, b, "HELLO")
	w := &flusherWriter{}
	var wgEnd sync.WaitGroup
	wgEnd.Add(1)
	var wgReady sync.WaitGroup
	wgReady.Add(1)

	end.Go(func() {
		wgReady.Done()
		n, err := b.WriteTo(w)
		ut.AssertEqual(t, 25, n)
		ut.AssertEqual(t, io.EOF, err)
		ut.AssertEqual(t, "helloHELLOHave a nice day", w.buf.String())
	})

	wgReady.Wait()
	writeOk(t, b, "Have a nice day")
	ut.AssertEqual(t, nil, b.Close())
	end.Wait()
}

func TestBufferWriteClosed(t *testing.T) {
	// While Write() is running and WriteTo() is hung on a reader, Close() is
	// called, causing Write() to abort early.
	s := MakeSequence(10)
	var end End
	b := MakeBuffer(10)
	h := &hookWriter{
		f: []func(){
			func() {
				s.Step(4)
			},
		},
	}

	end.Go(func() {
		s.Step(3)
		b.WriteTo(h)
		s.Step(8)
	})

	end.Go(func() {
		s.Step(1)
		writeOk(t, b, "Hi")
		s.Step(2)
		writeOk(t, b, "Ho")
		s.Step(6)
	})

	end.Go(func() {
		s.Step(5)
		ut.AssertEqual(t, nil, b.Close())
		s.Step(7)
	})

	s.Step(0)
	s.Step(9)
	end.Wait()
	ut.AssertEqual(t, "HiHo", h.buf.String())
	ut.AssertEqual(t, 0, len(h.f))
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

type hookWriter struct {
	buf bytes.Buffer
	f   []func()
}

func (h *hookWriter) Write(p []byte) (int, error) {
	fmt.Printf("hookWriter.Write()\n")
	h.f[0]()
	h.f = h.f[1:]
	return h.buf.Write(p)
}

type Sequence struct {
	wg []sync.WaitGroup
}

func MakeSequence(steps int) *Sequence {
	s := &Sequence{make([]sync.WaitGroup, steps)}
	for i := range s.wg {
		s.wg[i].Add(1)
	}
	return s
}

func (s *Sequence) Step(step int) {
	if step > 0 {
		s.wg[step-1].Wait()
	}
	s.wg[step].Done()
}

type End struct{ sync.WaitGroup }

func (e *End) Go(f func()) {
	e.Add(1)
	go func() {
		defer e.Done()
		f()
	}()
}
