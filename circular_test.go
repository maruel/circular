// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package circular

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maruel/ut"
)

func ExampleMakeBuffer() {
	// Normally, use a larger buffer.
	logBuffer := MakeBuffer(1024)

	// Normally, use an actual file.
	f, err := ioutil.TempFile("", "circular")
	if err != nil {
		panic(err)
	}
	defer func() {
		f.Close()
		if err := os.Remove(f.Name()); err != nil {
			panic(err)
		}
	}()

	// Sends to both circular buffer and file.
	log.SetOutput(io.MultiWriter(logBuffer, f))
	// Normally remove this call. Used here so output can be verified below.
	log.SetFlags(0)

	var wgDone sync.WaitGroup
	wgDone.Add(1)

	var wgReady sync.WaitGroup
	wgReady.Add(1)
	go func() {
		defer wgDone.Done()
		// This has to be done otherwise this goroutine may not even have the
		// chance of getting scheduled before the original function exits.
		wgReady.Done()
		// Asynchronously write to stdout. Normally, use os.Stderr.
		_, _ = logBuffer.WriteTo(os.Stdout)
	}()

	log.Printf("This line is served over HTTP; file and stdout")

	server := &http.Server{
		Addr: ":",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			// Streams the log buffer over HTTP until Close() is called. Data is
			// automatically flushed after a second. This is a trade-off about small
			// TCP packets causing lots of I/O overhead vs delay.
			_, _ = logBuffer.WriteTo(AutoFlush(w, time.Second))
		}),
	}
	go func() {
		_ = server.ListenAndServe()
	}()

	wgReady.Wait()

	// <DO ACTUAL WORK>

	// Flush ensures all readers have caught up before quitting.
	logBuffer.Flush()
	// Close gracefully closes the readers. This will properly TCP close the
	// connections.
	logBuffer.Close()

	wgDone.Wait()

	// Output:
	// This line is served over HTTP; file and stdout
}

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
	b := makeBuffer(10)
	for i, item := range data {
		writeOk(t, b, item.written)
		bytesWritten += len(item.written)
		ut.AssertEqualIndex(t, i, item.buf, string(b.buf))
		ut.AssertEqualIndex(t, i, int64(bytesWritten), b.writer.get())
	}
}

func TestBufferNoHangOnFail(t *testing.T) {
	b := MakeBuffer(10)
	writeOk(t, b, "hello")
	actualErr := errors.New("Failed")
	// Returns immediately.
	n, err := b.WriteTo(&failWriter{actualErr})
	ut.AssertEqual(t, int64(0), n)
	ut.AssertEqual(t, actualErr, err)
}

func TestBufferZero(t *testing.T) {
	ut.AssertEqual(t, (*buffer)(nil), MakeBuffer(0))
}

func TestBufferEOF(t *testing.T) {
	b := MakeBuffer(10)
	writeOk(t, b, "Hello")
	actualErr := errors.New("Failed")
	n, err := b.WriteTo(&failWriter{actualErr})
	ut.AssertEqual(t, int64(0), n)
	ut.AssertEqual(t, actualErr, err)
}

func TestBufferClosedTwice(t *testing.T) {
	b := MakeBuffer(10)
	ut.AssertEqual(t, nil, b.Close())
	ut.AssertEqual(t, io.ErrClosedPipe, b.Close())
}

func TestBufferClosedWrite(t *testing.T) {
	b := MakeBuffer(10)
	ut.AssertEqual(t, nil, b.Close())
	n, err := b.Write([]byte("foo"))
	ut.AssertEqual(t, 0, n)
	ut.AssertEqual(t, io.ErrClosedPipe, err)
}

func TestBufferClosedWriteTo(t *testing.T) {
	b := MakeBuffer(10)
	ut.AssertEqual(t, nil, b.Close())
	n, err := b.WriteTo(&bytes.Buffer{})
	ut.AssertEqual(t, int64(0), n)
	ut.AssertEqual(t, io.ErrClosedPipe, err)
}

func TestBufferWriteLock(t *testing.T) {
	b := makeBuffer(10)
	var end End
	var ready sync.WaitGroup
	b.writerLock.Lock()
	ready.Add(1)
	end.Go(func() {
		ready.Done()
		n, err := b.Write([]byte("Hello"))
		ut.AssertEqual(t, 0, n)
		ut.AssertEqual(t, io.ErrClosedPipe, err)
	})
	ready.Wait()
	ut.AssertEqual(t, nil, b.Close())
	b.writerLock.Unlock()
	end.Wait()
}

func TestBufferRolledOver(t *testing.T) {
	var end End
	b := makeBuffer(10)
	buffer := &bytes.Buffer{}
	writeOk(t, b, "abcdefghijklmnopqr")
	ut.AssertEqual(t, "klmnopqrij", string(b.buf))
	var wg sync.WaitGroup

	wg.Add(1)
	end.Go(func() {
		wg.Done()
		n, err := b.WriteTo(buffer)
		ut.AssertEqual(t, int64(10), n)
		ut.AssertEqual(t, io.EOF, err)
	})
	wg.Wait()
	writeOk(t, b, "stu")
	ut.AssertEqual(t, nil, b.Close())
	end.Wait()
	ut.AssertEqual(t, "ijklmnopqr", buffer.String())
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
			ut.AssertEqual(t, int64(20), n)
			ut.AssertEqual(t, io.EOF, err)
		}(i)
	}

	wgReady.Wait()
	writeOk(t, b, "abcde")
	writeOk(t, b, "fghij")
	writeOk(t, b, "klmnopqrstuvwxyzAB")

	ut.AssertEqual(t, nil, b.Close())
	wgEnd.Wait()

	for i := 0; i < len(buffers); i++ {
		ut.AssertEqual(t, "abcdefghijklmnopqrst", buffers[i].String())
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
		n, err := b.WriteTo(AutoFlush(w, 0))
		ut.AssertEqual(t, int64(7), n)
		ut.AssertEqual(t, io.EOF, err)
		ut.AssertEqual(t, "abcdefg", w.buf.String())
	})

	writeOk(t, b, "abcde")
	writeOk(t, b, "fg")
	wgReady.Wait()
	ut.AssertEqual(t, nil, b.Close())
	end.Wait()
	ut.AssertEqual(t, true, w.flushed)
}

func TestBufferFlusherRolledOver(t *testing.T) {
	var end End
	b := MakeBuffer(10)
	writeOk(t, b, "abcde")
	writeOk(t, b, "fghij")
	w := &flusherWriter{}
	var wgEnd sync.WaitGroup
	wgEnd.Add(1)
	var wgReady sync.WaitGroup
	wgReady.Add(1)

	end.Go(func() {
		wgReady.Done()
		n, err := b.WriteTo(AutoFlush(w, 0))
		ut.AssertEqual(t, int64(25), n)
		ut.AssertEqual(t, io.EOF, err)
		ut.AssertEqual(t, "abcdefghijklmnopqrstuvwxy", w.buf.String())
	})

	wgReady.Wait()
	writeOk(t, b, "klmnopqrstuvwxy")
	b.Flush()
	ut.AssertEqual(t, nil, b.Close())
	end.Wait()
}

func TestBufferWriteClosed(t *testing.T) {
	// While Write() is running and WriteTo() is hung on a reader, Close() is
	// called, causing Write() to abort early.
	s := makeSequence(10)
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
		_, _ = b.WriteTo(h)
		s.Step(8)
	})

	end.Go(func() {
		s.Step(1)
		writeOk(t, b, "ab")
		s.Step(2)
		writeOk(t, b, "cd")
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
	ut.AssertEqual(t, "abcd", h.buf.String())
	ut.AssertEqual(t, 0, len(h.f))
}

func TestBufferFlushEmpty(t *testing.T) {
	b := MakeBuffer(10)
	b.Flush()
	ut.AssertEqual(t, nil, b.Close())
	b.Flush()
}

func TestBufferFlushBlocking(t *testing.T) {
	var end End
	s := makeSequence(6)
	b := MakeBuffer(10)
	writeOk(t, b, "abcde")
	end.Go(func() {
		h := &hookWriter{
			f: []func(){
				func() {
					s.Step(2)
				},
			},
		}
		s.Step(0)
		_, _ = b.WriteTo(h)
		s.Step(4)
	})
	s.Step(1)
	b.Flush()
	s.Step(3)
	ut.AssertEqual(t, nil, b.Close())
	s.Step(5)
	end.Wait()
}

func TestBufferStressShort(t *testing.T) {
	stressTest(t, "Hey", func() io.ReadWriter { return &bytes.Buffer{} })
}

func TestBufferStressLong(t *testing.T) {
	stressTest(t, "Long string", func() io.ReadWriter { return &bytes.Buffer{} })
}

func TestBufferStressLongFlush(t *testing.T) {
	// The Flush() function hangs a bit to test more race conditions in that case.
	stressTest(t, "Long string", func() io.ReadWriter { return &flusherWriterDelay{} })
}

func stressTest(t *testing.T, s string, maker func() io.ReadWriter) {
	var wgStart sync.WaitGroup
	var wgReady sync.WaitGroup
	var endReaders End
	var endWriters End
	wgStart.Add(1)

	b := MakeBuffer(10)
	// Create same number of readers and writers.
	readers := make([]io.ReadWriter, ConcurrentRacers)
	for i := range readers {
		readers[i] = maker()
	}
	for i := 0; i < len(readers); i++ {
		wgReady.Add(1)
		endReaders.Add(1)
		go func(j int) {
			defer endReaders.Done()
			wgReady.Done()
			n, err := b.WriteTo(AutoFlush(readers[j], 1*time.Millisecond))
			ut.AssertEqual(t, int64(len(s)*len(readers)), n)
			ut.AssertEqual(t, io.EOF, err)
		}(i)

		wgReady.Add(1)
		endWriters.Go(func() {
			wgReady.Done()
			wgStart.Wait()
			writeOk(t, b, s)
		})
	}
	wgReady.Wait()
	// Fire all writers at once.
	wgStart.Done()
	endWriters.Wait()
	b.Flush()
	ut.AssertEqual(t, nil, b.Close())
	endReaders.Wait()
	expected := strings.Repeat(s, len(readers))
	for i := range readers {
		buf, err := ioutil.ReadAll(readers[i])
		ut.AssertEqualIndex(t, i, nil, err)
		ut.AssertEqualIndex(t, i, expected, string(buf))
	}
}

func TestBufferWriteClosedPipe(t *testing.T) {
	// Close while Write() is stuck due to an hung Flush().
	var end End
	s := makeSequence(3)
	b := MakeBuffer(10)
	w := &flusherWriterHang{}
	// Misallign the buffer in the middle.
	writeOk(t, b, "abcde")
	w.hang.Add(1)
	end.Go(func() {
		s.Step(0)
		n, err := b.WriteTo(AutoFlush(w, 0))
		// Never gets to write anything due to hang.
		ut.AssertEqual(t, int64(5), n)
		ut.AssertEqual(t, io.EOF, err)
	})
	end.Go(func() {
		s.Step(1)
		// 12 is larger than the size of the buffer.
		n, err := b.Write([]byte("fghijklmnopq"))
		// 2 bytes are lost.
		ut.AssertEqual(t, 10, n)
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
	b := makeBuffer(10)
	w := [2]flusherWriterHang{}
	writeOk(t, b, "abcdefghijk")
	w[0].hang.Add(1)
	end.Go(func() {
		// This one hangs.
		s.Step(0)
		n, err := b.WriteTo(AutoFlush(&w[0], 0))
		ut.AssertEqual(t, "bcdefghijklmnopqrst", w[0].buf.String())
		ut.AssertEqual(t, int64(19), n)
		ut.AssertEqual(t, io.EOF, err)
	})
	end.Go(func() {
		s.Step(1)
		n, err := b.WriteTo(AutoFlush(&w[1], 0))
		ut.AssertEqual(t, "bcdefghijklmnopqrstu", w[1].buf.String())
		ut.AssertEqual(t, int64(20), n)
		ut.AssertEqual(t, io.EOF, err)
	})
	end.Go(func() {
		s.Step(2)
		n, err := b.Write([]byte("lmnopqrstuvwxyzABC"))
		ut.AssertEqual(t, 10, n)
		ut.AssertEqual(t, io.ErrClosedPipe, err)
	})
	s.Step(3)
	// Spuriously wake up the Writer. This is to exercise the loop in Write()
	// that iterates over b.positions and finds a laggard.
	b.readers.broadcast()
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

// Utilities.

// writeOk writes and ensures write succeeded.
func writeOk(t *testing.T, w io.Writer, s string) {
	n, err := w.Write([]byte(s))
	ut.AssertEqual(t, len(s), n)
	ut.AssertEqual(t, nil, err)
}

// failWriter fails all writes.
type failWriter struct {
	err error
}

func (f *failWriter) Write(p []byte) (int, error) {
	return 0, f.err
}

// flusherWriter implements http.Flusher.
type flusherWriter struct {
	buf     bytes.Buffer
	flushed bool
}

func (f *flusherWriter) Read(p []byte) (int, error) {
	return f.buf.Read(p)
}

func (f *flusherWriter) Write(p []byte) (int, error) {
	return f.buf.Write(p)
}

func (f *flusherWriter) Flush() {
	f.flushed = true
}

// flusherWriterHang implements http.Flusher but hangs in addition.
type flusherWriterHang struct {
	flusherWriter
	hang sync.WaitGroup
}

func (f *flusherWriterHang) Flush() {
	f.hang.Wait()
	f.flusherWriter.Flush()
}

// flusherWriterDelay implements http.Flusher but induces a goroutine
// scheduling event in addition.
type flusherWriterDelay struct {
	flusherWriter
}

func (f *flusherWriterDelay) Flush() {
	// Induces a random thread switch to hang just a little.
	runtime.Gosched()
	f.flusherWriter.Flush()
}

// hookWriter pops a function from the hook slice at each write.
type hookWriter struct {
	buf bytes.Buffer
	f   []func()
}

func (h *hookWriter) Write(p []byte) (int, error) {
	h.f[0]()
	h.f = h.f[1:]
	return h.buf.Write(p)
}

// sequence is used to enforce deterministic behavior like a state machine.
type sequence struct {
	wg []sync.WaitGroup
}

func makeSequence(steps int) *sequence {
	s := &sequence{make([]sync.WaitGroup, steps)}
	for i := range s.wg {
		s.wg[i].Add(1)
	}
	return s
}

func (s *sequence) Step(step int) {
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
