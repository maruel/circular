// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// Package circular implements an efficient thread-safe circular byte buffer to
// keep in-memory logs. It implements both io.Writer and io.WriterTo.
//
// It carries a minimal set of dependencies.
package circular

import (
	"io"
	"sync"
	"sync/atomic"
)

// Buffer is designed to keep recent logs in-memory efficiently and in a
// thread-safe manner. It implements both io.Writer and io.WriterTo. It must be
// instantiated with MakeBuffer().
//
// No allocation while writing to it. Independent readers each have their read
// position and are synchronized with the writer to not have any data loss.
//
// BUG(maruel): Use range-lock instead of locking the whole buffer. This will
// permits blocking-less writes when readers are keeping up with the writer,
// that is, when the readers throughput is equal or larger to the writer
// throughput.
type Buffer struct {
	closed           int32          // set to 1 by Close() via atomic.
	wgReaders        sync.WaitGroup // number of readers active.
	wgReadersWaiting sync.WaitGroup // number of readers waiting for new data.
	wgWriterDone     sync.WaitGroup // safe for readers to get back asleep.
	lock             sync.RWMutex   // lock controls the following members.
	newData          *sync.Cond     // used to unblock readers all at once.
	buf              []byte         // the data itself.
	bytesWritten     int64          // total bytes written.
}

// MakeBuffer returns an initialized Buffer.
func MakeBuffer(size int) *Buffer {
	if size <= 0 {
		return nil
	}
	b := &Buffer{
		buf: make([]byte, size),
	}
	b.newData = sync.NewCond(b.lock.RLocker())
	return b
}

// Write implements io.Writer.
//
// If p is longer or equal to the internal buffer, this call will block until
// all readers have kept up with the data. To not get into this condition and
// keep Write() performant, ensure the internal buffer is significantly larger
// than the largest writes.
func (b *Buffer) Write(p []byte) (int, error) {
	s := int64(len(b.buf))
	if s == 0 {
		// Wasn't instantiated with MakeBuffer().
		return 0, io.ErrClosedPipe
	}
	if atomic.LoadInt32(&b.closed) != 0 {
		return 0, io.ErrClosedPipe
	}

	b.lock.Lock()
	defer b.lock.Unlock()
	originalbytesWritten := b.bytesWritten
	for chunkSize := int64(len(p)); chunkSize != 0; chunkSize = int64(len(p)) {
		writeOffset := b.bytesWritten % s
		if chunkSize > s-writeOffset {
			chunkSize = s - writeOffset
		}
		copy(b.buf[writeOffset:], p[:chunkSize])
		b.bytesWritten += chunkSize
		p = p[chunkSize:]

		b.wakeAllReaders()

		if atomic.LoadInt32(&b.closed) != 0 {
			// BUG(maruel): Could overflow on 32 bits platforms.
			return int(b.bytesWritten - originalbytesWritten), io.ErrClosedPipe
		}
	}
	// BUG(maruel): Could overflow on 32 bits platforms.
	return int(b.bytesWritten - originalbytesWritten), nil
}

// Close implements io.Closer. It closes all WriteTo() streamers synchronously
// and blocks until they all quit.
func (b *Buffer) Close() error {
	if !atomic.CompareAndSwapInt32(&b.closed, 0, 1) {
		return io.ErrClosedPipe
	}
	b.newData.Broadcast()
	b.wgReadersWaiting.Wait()
	// Wait for all readers to be done.
	b.wgReaders.Wait()
	return nil
}

func (b *Buffer) wakeAllReaders() {
	// Carefully tuned locking sequence to ensure all readers caught up.
	b.wgWriterDone.Add(1)
	b.lock.Unlock()
	b.newData.Broadcast()
	b.wgReadersWaiting.Wait()
	b.wgWriterDone.Done()
	b.lock.Lock()
}

// flusher is the equivalent of http.Flusher. Importing net/http is quite
// heavy, so it's not worth importing it just for this interface.
type flusher interface {
	Flush()
}

// WriteTo implements io.WriterTo. It streams a Buffer to a io.Writer until
// the Buffer is closed. It forcibly flushes the output if w supports
// http.Flusher so it is sent to the underlying TCP connection as data is
// appended.
func (b *Buffer) WriteTo(w io.Writer) (int, error) {
	b.wgReaders.Add(1)
	defer b.wgReaders.Done()

	f, _ := w.(flusher)
	s := int64(len(b.buf))
	var err error
	readOffset := int64(0)

	b.lock.RLock()

	if b.bytesWritten > s {
		// Had rolled over already, initial data is lost.
		readOffset = b.bytesWritten - s
	}
	originalReadOffset := readOffset

	// One of the important property is that when the Buffer is quickly written
	// to then closed, the remaining data is still sent to all readers.
	var wgFlushing sync.WaitGroup
	for (atomic.LoadInt32(&b.closed) == 0 || readOffset != b.bytesWritten) && err == nil {
		wrote := false
		for readOffset < b.bytesWritten && err == nil {
			off := readOffset % s
			end := (b.bytesWritten % s)
			if end <= off {
				end = s
			}
			// Never call .Write() and .Flush() concurrently.
			wgFlushing.Wait()
			var n int
			n, err = w.Write(b.buf[off:end])
			readOffset += int64(n)
			wrote = n != 0
			if n == 0 {
				break
			}
		}
		if f != nil && wrote {
			wgFlushing.Add(1)
			go func() {
				defer wgFlushing.Done()
				// Flush concurrently to the writer.
				f.Flush()
			}()
		}
		if err != nil {
			break
		}
		b.wgWriterDone.Wait()
		b.wgReadersWaiting.Add(1)
		b.newData.Wait()
		b.wgReadersWaiting.Done()
	}
	b.lock.RUnlock()
	if err == nil {
		err = io.EOF
	}
	wgFlushing.Wait()
	// BUG(maruel): Could overflow on 32 bits platforms.
	return int(readOffset - originalReadOffset), err
}
