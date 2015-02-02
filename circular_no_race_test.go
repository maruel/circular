// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// +build !race

package circular

import (
	"io"
	"testing"

	"github.com/maruel/ut"
)

// Racy behavior only starts to show with large number of concurrent
// goroutines, generally in the range of 256~512. On the other hand this
// makes go test -race unbearably slow.
const ConcurrentRacers = 512

// TODO(maruel): These tests are racy and need to be fixed.

func TestBufferWriteClosedPipe(t *testing.T) {
	// Close while Write() is stuck due to an hung Flush().
	var end End
	s := makeSequence(3)
	b := MakeBuffer(10)
	w := &flusherWriterHang{}
	writeOk(t, b, "Hello")
	w.hang.Add(1)
	end.Go(func() {
		s.Step(0)
		n, err := b.WriteTo(AutoFlushInstant(w))
		ut.AssertEqual(t, 10, n)
		ut.AssertEqual(t, io.EOF, err)
	})
	end.Go(func() {
		s.Step(1)
		n, err := b.Write([]byte("Overflowed"))
		ut.AssertEqual(t, 5, n)
		ut.AssertEqual(t, io.ErrClosedPipe, err)
	})
	s.Step(2)
	w.hang.Done()
	ut.AssertEqual(t, nil, b.Close())
	end.Wait()
}

func TestBufferReaderLaggard(t *testing.T) {
	// Synthetically makes a reader a laggard, close while still writing.
	var end End
	s := makeSequence(6)
	b := MakeBuffer(10)
	w := [2]flusherWriterHang{}
	writeOk(t, b, "HelloWorld!")
	w[0].hang.Add(1)
	end.Go(func() {
		s.Step(0)
		n, err := b.WriteTo(AutoFlushInstant(&w[0]))
		ut.AssertEqual(t, "elloWorld!Overflowe", w[0].buf.String())
		ut.AssertEqual(t, 19, n)
		ut.AssertEqual(t, io.EOF, err)
	})
	end.Go(func() {
		s.Step(1)
		n, err := b.WriteTo(AutoFlushInstant(&w[1]))
		ut.AssertEqual(t, "elloWorld!Overflowe", w[1].buf.String())
		ut.AssertEqual(t, 19, n)
		ut.AssertEqual(t, io.EOF, err)
	})
	end.Go(func() {
		s.Step(2)
		n, err := b.Write([]byte("Overflowed Further"))
		ut.AssertEqual(t, 9, n)
		ut.AssertEqual(t, io.ErrClosedPipe, err)
	})
	s.Step(3)
	// Spuriously wake up the Writer. This is to exercise the loop in Write()
	// that iterates over b.readerPositions and finds a laggard.
	b.wgReaderWorked.Broadcast()
	end.Go(func() {
		s.Step(4)
		ut.AssertEqual(t, nil, b.Close())
	})
	end.Go(func() {
		s.Step(5)
		w[0].hang.Done()
	})
	end.Wait()
}
