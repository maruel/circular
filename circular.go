// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// Package circular implements an efficient thread-safe circular byte buffer to
// keep in-memory logs. It implements both io.Writer and io.WriterTo.
package circular

import (
	"io"
	"net/http"
	"sync"
)

// Buffer is designed to keep in memory recent logs. It implements both
// io.Writer and io.WriterTo. It must be instantiated with MakeBuffer().
type Buffer struct {
	wg          sync.WaitGroup
	lock        sync.Mutex
	newData     *sync.Cond
	buf         []byte
	closed      bool
	rolledOver  bool // if true, reading should start at .writeOffset, otherwise it should start at 0.
	writeOffset int
}

// MakeBuffer returns a thread-safe Buffer that makes no allocation while
// writing to it. Independent readers each have their read position.
func MakeBuffer(size int) *Buffer {
	if size <= 1 {
		return nil
	}
	b := &Buffer{
		buf: make([]byte, size),
	}
	b.newData = sync.NewCond(&b.lock)
	return b
}

// Write implements io.Writer.
func (b *Buffer) Write(p []byte) (int, error) {
	n := len(p)
	s := len(b.buf)
	if s == 0 {
		// Wasn't instantiated with MakeBuffer().
		return 0, io.ErrClosedPipe
	}

	b.lock.Lock()
	defer b.lock.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	if n >= s {
		// BUG(maruel): If p is longer or equal to the internal buffer, the
		// begining of p is lost. This implementation prefers performance to data
		// loss, otherwise the Write() call would hang until all readers are done.
		// Next version will correct this, or make this behavior optional.
		p = p[n-s+1:]
		n = len(p)
	}
	copy(b.buf[b.writeOffset:], p)
	if b.writeOffset+n >= s {
		// Roll over.
		copy(b.buf, p[s-b.writeOffset:])
		b.rolledOver = true
	}
	b.writeOffset = (b.writeOffset + n) % s

	b.newData.Broadcast()
	return len(p), nil
}

// Close implements io.Closer. It closes all WriteTo() streamers synchronously.
func (b *Buffer) Close() error {
	inner := func() error {
		b.lock.Lock()
		defer b.lock.Unlock()
		if b.closed {
			return io.ErrClosedPipe
		}
		b.closed = true
		b.newData.Broadcast()
		return nil
	}

	err := inner()
	// Wait for all readers to be unblocked.
	b.wg.Wait()
	return err
}

// WriteTo implements io.WriterTo. It streams a Buffer to a io.Writer until
// the Buffer is clsoed. It forcibly flushes the output if w supports
// http.Flusher so it is sent to the underlying TCP connection as data is
// appended.
func (b *Buffer) WriteTo(w io.Writer) (int, error) {
	b.wg.Add(1)
	defer b.wg.Done()

	f, _ := w.(http.Flusher)
	flush := func() {
		// It is important to not keep the lock while flushing! Otherwise this
		// would block the writer.
		b.lock.Unlock()
		defer b.lock.Lock()
		f.Flush()
	}

	s := len(b.buf)
	total := 0
	var err error

	b.lock.Lock()
	defer b.lock.Unlock()
	rolledOver := b.rolledOver
	readOffset := 0
	if rolledOver {
		// The buffer had already rolled over when this reader started.
		readOffset = b.writeOffset
	}

	// One of the important property is that when the Buffer is quickly written
	// to then closed, the remaining data is still sent to all readers.
	for (!b.closed || rolledOver || readOffset != b.writeOffset) && err == nil {
		var m, n int
		for (rolledOver || readOffset > b.writeOffset) && err == nil {
			// Missed a roll over. Support partial writes.
			m, err = w.Write(b.buf[readOffset:])
			readOffset = (readOffset + m) % s
			total += m
			if readOffset == 0 {
				rolledOver = false
			}
		}
		for (readOffset < b.writeOffset) && err == nil {
			n, err = w.Write(b.buf[readOffset:b.writeOffset])
			readOffset = (readOffset + n) % s
			total += n
		}
		if err != nil {
			break
		}
		if f != nil && (m != 0 || n != 0) {
			flush()
		}
		if b.closed && readOffset == b.writeOffset {
			break
		}
		b.newData.Wait()
	}
	if err == nil {
		err = io.EOF
	}
	return total, err
}
