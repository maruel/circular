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

func ExampleMakeBuffer_asynchronous() {
	// Saves the log to disk asynchrously. The main drawback, which is
	// significant with this use-case, is that logs will be partially lost on
	// panic().
	logBuffer := MakeBuffer(10 * 1024 * 1024)
	log.SetFlags(0)
	log.SetOutput(logBuffer)
	log.Printf("This line is not lost")
	f, err := os.OpenFile("app.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		panic(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		wg.Done()
		_, _ = logBuffer.WriteTo(f)
	}()
	wg.Wait()
	log.Printf("One more line")
	logBuffer.Flush()
	logBuffer.Close()
	f.Close()
	out, err := ioutil.ReadFile("app.log")
	if err != nil {
		panic(err)
	}
	_ = os.Remove("app.log")
	_, _ = os.Stdout.Write(out)
	// Output:
	// This line is not lost
	// One more line
}

func ExampleMakeBuffer_synchronous() {
	// Safely writes to disk synchronously in addition to keeping a circular
	// buffer.
	logBuffer := MakeBuffer(10 * 1024 * 1024)
	log.SetFlags(0)
	log.SetOutput(logBuffer)
	log.Printf("This line is lost!")
	f, err := os.OpenFile("app.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		panic(err)
	}
	log.SetOutput(io.MultiWriter(logBuffer, f))
	log.Printf("One more line")
	logBuffer.Close()
	f.Close()
	out, err := ioutil.ReadFile("app.log")
	if err != nil {
		panic(err)
	}
	_ = os.Remove("app.log")
	_, _ = os.Stdout.Write(out)
	// Output:
	// One more line
}

func ExampleMakeBuffer_stdout() {
	// Prints the content asynchronously to stdout or stderr in addition to
	// keeping the log in memory.
	logBuffer := MakeBuffer(10 * 1024 * 1024)
	log.SetFlags(0)
	log.SetOutput(logBuffer)
	log.Printf("This line is not lost")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		wg.Done()
		_, _ = logBuffer.WriteTo(os.Stdout)
	}()
	wg.Wait()
	log.Printf("One more line")
	logBuffer.Close()
	// Output:
	// This line is not lost
	// One more line
}

func ExampleMakeBuffer_web() {
	// Serves the circular log buffer over HTTP. This can be coupled with the
	// other techniques.
	logBuffer := MakeBuffer(10 * 1024 * 1024)
	log.SetFlags(log.LstdFlags)
	log.SetOutput(logBuffer)
	log.Printf("This line is not lost")
	http.HandleFunc("/",
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = logBuffer.WriteTo(w)
		})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		wg.Done()
		_ = http.ListenAndServe(":6060", nil)
	}()
	wg.Wait()
	log.Printf("One more line")
	logBuffer.Close()
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
	b := MakeBuffer(10)
	for i, item := range data {
		writeOk(t, b, item.written)
		bytesWritten += len(item.written)
		ut.AssertEqualIndex(t, i, item.buf, string(b.buf))
		ut.AssertEqualIndex(t, i, int64(bytesWritten), b.bytesWritten)
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
	n, err = b.WriteTo(&bytes.Buffer{})
	ut.AssertEqual(t, 0, n)
	ut.AssertEqual(t, io.ErrClosedPipe, err)
	ut.AssertEqual(t, io.ErrClosedPipe, b.Close())
	b.Flush()
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

func TestBufferWriteLock(t *testing.T) {
	b := MakeBuffer(10)
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
	b := MakeBuffer(10)
	buffer := &bytes.Buffer{}
	writeOk(t, b, "Overflowed totally")
	ut.AssertEqual(t, " totallyed", string(b.buf))
	var wg sync.WaitGroup

	wg.Add(1)
	end.Go(func() {
		wg.Done()
		n, err := b.WriteTo(buffer)
		ut.AssertEqual(t, 13, n)
		ut.AssertEqual(t, io.EOF, err)
	})
	wg.Wait()
	// Technically, we want to wait for b.WriteTo() to get the lock. Argh.
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

	ut.AssertEqual(t, nil, b.Close())
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
		n, err := b.WriteTo(AutoFlushInstant(w))
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
		n, err := b.WriteTo(AutoFlushInstant(w))
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
	writeOk(t, b, "hello")
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
			ut.AssertEqual(t, len(s)*len(readers), n)
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
	ut.AssertEqual(t, nil, b.Close())
	endReaders.Wait()
	expected := strings.Repeat(s, len(readers))
	for i := range readers {
		buf, err := ioutil.ReadAll(readers[i])
		ut.AssertEqualIndex(t, i, nil, err)
		ut.AssertEqualIndex(t, i, expected, string(buf))
	}
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

func (f *flusherWriter) Read(p []byte) (int, error) {
	return f.buf.Read(p)
}

func (f *flusherWriter) Write(p []byte) (int, error) {
	return f.buf.Write(p)
}

// Flush implements http.Flusher.
func (f *flusherWriter) Flush() {
	f.flushed = true
}

type flusherWriterHang struct {
	flusherWriter
	hang sync.WaitGroup
}

func (f *flusherWriterHang) Flush() {
	f.hang.Wait()
	f.flusherWriter.Flush()
}

type flusherWriterDelay struct {
	flusherWriter
}

func (f *flusherWriterDelay) Flush() {
	// Induces a random thread switch to hang just a little.
	runtime.Gosched()
	f.flusherWriter.Flush()
}

type hookWriter struct {
	buf bytes.Buffer
	f   []func()
}

func (h *hookWriter) Write(p []byte) (int, error) {
	h.f[0]()
	h.f = h.f[1:]
	return h.buf.Write(p)
}

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
