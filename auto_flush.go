// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package circular

import (
	"io"
	"sync"
	"time"
)

// WriteFlusher is a io.Writer that can also be flushed. It is compatible with
// http.Flusher.
type WriteFlusher interface {
	io.Writer

	Flush()
}

// AutoFlush converts an io.Writer supporting http.Flusher to call Flush()
// automatically after each write after a small delay.
//
// To flush after each Write() call, pass 0 as delay.
//
// The main use case is http connection when piping a circular buffer to it.
func AutoFlush(w io.Writer, delay time.Duration) WriteFlusher {
	f, _ := w.(flusher)
	if f == nil {
		return noOpFlush{w}
	}
	return &autoFlusher{w: w, f: f, delay: delay}
}

// Internal details.

// flusher is the equivalent of http.Flusher. Importing net/http is quite
// heavy. It's not worth importing it just for this interface which is
// guaranteed to be stable for Go v1.x.
type flusher interface {
	Flush()
}

type noOpFlush struct {
	io.Writer
}

func (n noOpFlush) Flush() {
	// For some reason, calls to this function are not caught by go test -cover.
}

type autoFlusher struct {
	f            flusher
	w            io.Writer
	delay        time.Duration
	lock         sync.Mutex
	flushPending bool
}

func (a *autoFlusher) Write(p []byte) (int, error) {
	// Never call .Write() and .Flush() concurrently.
	a.lock.Lock()
	n, err := a.w.Write(p)
	defer a.lock.Unlock()
	if n != 0 && !a.flushPending {
		a.flushPending = true
		go func() {
			if a.delay != 0 {
				<-time.After(a.delay)
			}
			a.lock.Lock()
			if a.flushPending {
				a.f.Flush()
				a.flushPending = false
			}
			a.lock.Unlock()
		}()
	}
	return n, err
}

func (a *autoFlusher) Flush() {
	a.lock.Lock()
	defer a.lock.Unlock()
	a.flushPending = false
	a.f.Flush()
}
