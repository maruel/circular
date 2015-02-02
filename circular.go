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
	"time"
)

// Buffer is designed to keep recent logs in-memory efficiently and in a
// thread-safe manner. It implements both io.Writer and io.WriterTo. It must be
// instantiated with MakeBuffer().
//
// No heap allocation is done within Buffer.Write(). Independent readers each
// have their read position and are synchronized with the writer to not have
// any data loss.
type Buffer struct {
	closed              int32          // Set to 1 by Close() via atomic.
	wgActiveReaders     sync.WaitGroup // Number of readers active.
	wgReaderWorked      sync.Cond      // Number of readers waiting for new data.
	readerPositionsLock sync.Mutex     // Protects the next 2 variables.
	readerPositions     map[int]int64  // Position of each readers. Can only be changed with readersLock.RLock() held. It's to ensure the writer doesn't not overwrite bytes.
	nextReaderID        int            // Next id for the next reader, b.WriteTo().
	writerLock          sync.Mutex     // Only one writer can write chunks at once.
	readersLock         sync.RWMutex   // Lock controls the following members.
	newData             sync.Cond      // Used to unblock readers all at once.
	buf                 []byte         // The data itself.
	bytesWritten        int64          // Total bytes written. It's controlled by readersLock.Lock(). b.bytesWritten % len(b.buf) represents the write offset. This value monotonically increases.
}

// MakeBuffer returns an initialized Buffer.
func MakeBuffer(size int) *Buffer {
	if size <= 0 {
		return nil
	}
	b := &Buffer{
		buf:             make([]byte, size),
		readerPositions: map[int]int64{},
	}
	b.wgReaderWorked = *sync.NewCond(&sync.Mutex{})
	b.newData = *sync.NewCond(b.readersLock.RLocker())
	return b
}

// Write implements io.Writer. It appends data to the buffer.
//
// If p is longer or equal to the internal buffer, this call will block until
// all readers have kept up with the data. To not get into this condition and
// keep Write() performant, ensure the internal buffer is significantly larger
// than the largest writes.
func (b *Buffer) Write(p []byte) (int, error) {
	s := int64(len(b.buf))
	if s == 0 || atomic.LoadInt32(&b.closed) != 0 {
		return 0, io.ErrClosedPipe
	}

	// Only one writer can write multiple chunks at once. This fixes the issue
	// where multiple concurrent writers would have their chunk interleaved.
	b.writerLock.Lock()
	defer b.writerLock.Unlock()

	if atomic.LoadInt32(&b.closed) != 0 {
		return 0, io.ErrClosedPipe
	}

	// Takes the RWLock writer lock. At that point, it's safe to write data.
	b.readersLock.Lock()
	defer b.readersLock.Unlock()

	originalbytesWritten := b.bytesWritten
	for len(p) > 0 && atomic.LoadInt32(&b.closed) == 0 {
		writeOffset := b.bytesWritten % s
		chunkSize := int64(len(p))
		if chunkSize > s-writeOffset {
			chunkSize = s - writeOffset
		}
		skip := false
		if b.bytesWritten >= s {
			bytesToBeOverwritten := b.bytesWritten + chunkSize - s
			b.readerPositionsLock.Lock()
			// Look for a laggard. In that case, sleep until it catches up.
			for _, readerPosition := range b.readerPositions {
				if readerPosition < bytesToBeOverwritten {
					// Damn, we have to sleep again.
					skip = true
					break
				}
			}
			b.readerPositionsLock.Unlock()
		}

		if !skip {
			copy(b.buf[writeOffset:], p)
			// Use atomic function for concurrency with Flush(). In practice
			// b.readerPositionsLock could as well be used. It's not a big deal
			// either way.
			atomic.StoreInt64(&b.bytesWritten, b.bytesWritten+chunkSize)
			p = p[chunkSize:]
		}

		// Only do the lock/unlock dance if there's actually at least one reader.
		if len(b.readerPositions) != 0 {
			// Readers are now fired.
			b.readersLock.Unlock()
			b.newData.Broadcast()
			// Wait for at least one reader to work first.
			b.wgReaderWorked.L.Lock()
			b.wgReaderWorked.Wait()
			b.wgReaderWorked.L.Unlock()
			// Ensure no other reader get in the way.
			b.readersLock.Lock()
		}
	}
	diff := int(b.bytesWritten - originalbytesWritten)
	if atomic.LoadInt32(&b.closed) != 0 {
		return diff, io.ErrClosedPipe
	}
	return diff, nil
}

// Close implements io.Closer. It closes all WriteTo() streamers synchronously
// and blocks until they all quit.
//
// Close can safely be called in a reentrant context, as it doesn't use any
// lock.
func (b *Buffer) Close() error {
	if len(b.buf) == 0 || !atomic.CompareAndSwapInt32(&b.closed, 0, 1) {
		return io.ErrClosedPipe
	}
	b.newData.Broadcast()
	// Waits for all the readers (active WriteTo() calls) to terminate. This can
	// be problematic when a reader is hung in a Flush() call.
	b.wgActiveReaders.Wait()
	return nil
}

// Flush implements http.Flusher. It forcibly blocks until all readers are up
// to date.
func (b *Buffer) Flush() {
	if len(b.buf) == 0 {
		return
	}
	for {
		if atomic.LoadInt32(&b.closed) != 0 {
			return
		}

		b.readerPositionsLock.Lock()
		if len(b.readerPositions) == 0 {
			return
		}
		bytesWritten := atomic.LoadInt64(&b.bytesWritten)
		laggard := false
		for _, readerPosition := range b.readerPositions {
			if readerPosition < bytesWritten {
				laggard = true
				break
			}
		}
		b.readerPositionsLock.Unlock()
		if !laggard || atomic.LoadInt32(&b.closed) != 0 {
			return
		}

		// There's a laggard, wait for an update.
		b.wgReaderWorked.L.Lock()
		b.wgReaderWorked.Wait()
		b.wgReaderWorked.L.Unlock()
	}
}

// WriteTo implements io.WriterTo. It streams a Buffer to a io.Writer until
// the Buffer is closed or a write error occurs.
//
// It forcibly flushes the output if w supports http.Flusher so it is sent to
// the underlying TCP connection as data is appended.
//
// This function can easily overflow on 32 bits platforms. In that case, return
// the largest valid integer.
func (b *Buffer) WriteTo(w io.Writer) (int, error) {
	if len(b.buf) == 0 || atomic.LoadInt32(&b.closed) != 0 {
		return 0, io.ErrClosedPipe
	}

	b.wgActiveReaders.Add(1)
	defer b.wgActiveReaders.Done()

	s := int64(len(b.buf))
	var err error
	bytesRead := int64(0)
	bytesLost := int64(0)

	b.readersLock.RLock()

	// Calculate the number of buffer cycles that were lost.
	if b.bytesWritten > s {
		bytesLost = b.bytesWritten - s
		bytesRead = bytesLost
	}
	b.readerPositionsLock.Lock()
	id := b.nextReaderID
	b.nextReaderID++
	b.readerPositions[id] = bytesLost
	b.readerPositionsLock.Unlock()

	// One of the important property is that when the Buffer is quickly written
	// to then closed, the remaining data is still sent to all readers.
	for atomic.LoadInt32(&b.closed) == 0 || bytesRead != b.bytesWritten {
		wrote := false
		for bytesRead < b.bytesWritten && err == nil {
			off := bytesRead % s
			end := (b.bytesWritten % s)
			if end <= off {
				end = s
			}

			var n int
			n, err = w.Write(b.buf[off:end])
			bytesRead += int64(n)
			b.readerPositionsLock.Lock()
			b.readerPositions[id] = bytesRead
			b.readerPositionsLock.Unlock()
			wrote = n != 0
			if n == 0 {
				break
			}
		}
		if wrote {
			// Wake up the writer so that it can rescan b.readerPositions.
			b.wgReaderWorked.Broadcast()

		}
		if err != nil || atomic.LoadInt32(&b.closed) != 0 {
			break
		}

		// Enter sleep mode, releasing the lock.
		b.newData.Wait()
	}
	b.readerPositionsLock.Lock()
	delete(b.readerPositions, id)
	b.readerPositionsLock.Unlock()
	b.readersLock.RUnlock()
	if err == nil {
		err = io.EOF
	}

	const maxInt = int64(int(^uint(0) >> 1))
	read := bytesRead - bytesLost
	if read > maxInt {
		// This line is hard to cover.
		read = maxInt
	}
	return int(read), err
}

// AutoFlush converts an io.Writer supporting http.Flusher to call Flush()
// automatically after each write after a small delay.
//
// The main use case is http connection when piping a circular buffer to it.
func AutoFlush(w io.Writer, delay time.Duration) io.Writer {
	f, _ := w.(flusher)
	if f == nil {
		return w
	}
	return &autoFlusher{w: w, f: f, delay: delay}
}

func AutoFlushInstant(w io.Writer) io.Writer {
	return AutoFlush(w, time.Duration(0))
}

// Internal details.

// flusher is the equivalent of http.Flusher. Importing net/http is quite
// heavy. It's not worth importing it just for this interface which is
// guaranteed to be stable for Go v1.x.
type flusher interface {
	Flush()
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
