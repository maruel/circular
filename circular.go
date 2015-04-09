// Copyright 2015 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

// Package circular implements an efficient thread-safe circular byte buffer to
// keep in-memory logs.
package circular

import (
	"io"
	"math"
	"sync"
	"sync/atomic"
)

// CircularBuffer is the interface to a circular buffer. It can be written to
// and read from concurrently.
//
// It is expected that objects implementing this interface are thread-safe.
type CircularBuffer interface {
	// Close() aborts all WriteTo() streamers synchronously and blocks until they
	// all quit. It also aborts in-progress Write() calls. Close() can safely be
	// called in a reentrant context, as it doesn't use any lock.
	//
	// If the caller wants the data to be flushed to all readers, Flush() must be
	// called first.
	io.Closer

	// Write(p) writes the data to the circular buffer and wakes up any WriteTo()
	// reader. If p is longer or equal to the internal buffer, this call will
	// block until all readers have kept up with the data. To not get into this
	// condition and keep Write() performant, ensure the internal buffer is
	// significantly larger than the largest writes.
	io.Writer

	// WriteTo() streams a buffer to a io.Writer until the buffer is closed or a
	// write error occurs. It forcibly flushes the output if w supports
	// http.Flusher so it is sent to the underlying TCP connection as data is
	// appended.
	io.WriterTo

	// Flush implements http.Flusher. It forcibly blocks until all readers are up
	// to date.
	Flush()
}

type buffer struct {
	closed     bit           // Set by Close().
	readers    positions     // Each reader position.
	writer     writePosition // Writer position.
	writerLock sync.Mutex    // Only one writer can write chunks at once.
	buf        []byte        // The data itself.
}

// MakeBuffer returns an initialized CircularBuffer.
//
// It is designed to keep recent logs in-memory efficiently and in a
// thread-safe manner. No heap allocation is done within Write(). Independent
// readers each have their read position and are synchronized with the writer
// to not have any data loss.
func MakeBuffer(size int) CircularBuffer {
	return makeBuffer(size)
}

func makeBuffer(size int) *buffer {
	if size <= 0 {
		return nil
	}
	b := &buffer{
		buf: make([]byte, size),
		readers: positions{
			cond: *sync.NewCond(&sync.Mutex{}),
			pos:  make(map[posID]int64),
		},
		writer: writePosition{
			cond: *sync.NewCond(&sync.Mutex{}),
		},
	}
	return b
}

func (b *buffer) Close() error {
	if !b.closed.set() {
		return io.ErrClosedPipe
	}
	b.readers.broadcast()
	b.writer.broadcast()
	// Waits for all the readers (active WriteTo() calls) to terminate.
	for b.readers.wait(math.MaxInt64-1) != math.MaxInt64 {
	}
	return nil
}

func (b *buffer) Write(p []byte) (int, error) {
	s := len(b.buf)
	s64 := int64(s)

	// Only one writer can write multiple chunks at once. This fixes the issue
	// where multiple concurrent writers would have their chunk interleaved.
	b.writerLock.Lock()
	defer b.writerLock.Unlock()

	originalbytesWritten := b.writer.get()
	for len(p) > 0 && !b.closed.isSet() {
		// write offset within the buffer.
		bytesWrittenWrapped := int(b.writer.get() % s64)
		chunkSize := len(p)
		if chunkSize > s-bytesWrittenWrapped {
			// wrapping around.
			chunkSize = s - bytesWrittenWrapped
		}
		// Look for a laggard. In that case, sleep until it catches up.
		//
		// Case #1:              01234567890123456789
		//  b.buf:               |--------|         10
		//  b.bytesWritten:      |---------------|  16
		//  bytesWrittenWrapped:       |             6
		//  laggardReader:       |------------|     13
		//  laggardWrapped:         |                3
		//  writeable section:   |-|   |--|         (0-2) & (6-9)
		//  danger zone:            |-|             (3-5)
		//
		// Case #2:              0123456789012345678901
		//  b.buf:               |--------|              10
		//  b.bytesWritten:      |--------------------|  21
		//  bytesWrittenWrapped:  |                       1
		//  laggardReader:       |-------------|         14
		//  laggardWrapped:          |                    3
		//  writeable section:    |-|                    (1-3)
		//  danger zone:         |   |----|              (0-0) & (4-9)
		laggardReader := b.readers.minPos()
		if laggardReader != math.MaxInt64 {
			if laggardReader <= b.writer.get()-s64 {
				// More than or equal a buffer size in difference.
				b.readers.wait(laggardReader)
				continue
			}

			laggardWrapped := int(laggardReader % s64)
			if laggardWrapped > bytesWrittenWrapped {
				delta := laggardWrapped - bytesWrittenWrapped
				if chunkSize > delta {
					chunkSize = delta
				}
			}
		}
		copy(b.buf[bytesWrittenWrapped:], p[:chunkSize])
		b.writer.add(chunkSize)
		p = p[chunkSize:]
	}
	diff := int(b.writer.get() - originalbytesWritten)
	if b.closed.isSet() {
		return diff, io.ErrClosedPipe
	}
	return diff, nil
}

func (b *buffer) Flush() {
	b.writerLock.Lock()
	defer b.writerLock.Unlock()

	laggardReader := b.readers.minPos()
	for !b.closed.isSet() && laggardReader != b.writer.get() && laggardReader != math.MaxInt64 {
		laggardReader = b.readers.wait(laggardReader)
	}
}

func (b *buffer) WriteTo(w io.Writer) (int64, error) {
	if b.closed.isSet() {
		return 0, io.ErrClosedPipe
	}

	// Calculate the number of buffer cycles that were lost.
	s := int64(len(b.buf))
	readOffset := int64(0)
	bytesLost := int64(0)
	bytesWritten := b.writer.get()
	if bytesWritten > s {
		bytesLost = bytesWritten - s
		readOffset = bytesLost
	}

	id := b.readers.register(bytesLost)
	defer b.readers.unregister(id)

	var err error
	for err == nil {
		for err == nil {
			if b.closed.isSet() {
				err = io.EOF
				break
			}
			bytesWritten = b.writer.get()
			if readOffset == bytesWritten {
				break
			}
			readOffsetWrapped := readOffset % s
			end := bytesWritten % s
			if end <= readOffsetWrapped {
				end = s
			}
			var n int
			n, err = w.Write(b.buf[readOffsetWrapped:end])
			readOffset = b.readers.add(id, n)
		}
		if err != nil {
			break
		}
		// Enter sleep mode.
		b.writer.wait(bytesWritten)
	}
	return readOffset - bytesLost, err
}

// Internal details.

type posID int

// positions wraps the position updates.
type positions struct {
	cond   sync.Cond
	pos    map[posID]int64
	nextID posID
}

func (p *positions) minPos() int64 {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	return p.minPosImpl()
}

// minPosImpl returns the minimum position value or math.MaxInt64. Lock must be
// held.
func (p *positions) minPosImpl() int64 {
	min := int64(math.MaxInt64)
	for _, v := range p.pos {
		if v < min {
			min = v
		}
	}
	return min
}

// wait waits until all positions are past prev. Returns math.MaxInt64 if there
// is no reader.
func (p *positions) wait(prev int64) int64 {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	for {
		min := p.minPosImpl()
		if min > prev {
			return min
		}
		p.cond.Wait()
	}
}

func (p *positions) add(id posID, chunkSize int) int64 {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	p.pos[id] += int64(chunkSize)
	p.broadcast()
	return p.pos[id]
}

func (p *positions) broadcast() {
	p.cond.Broadcast()
}

func (p *positions) register(initial int64) posID {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	cur := p.nextID
	p.pos[cur] = initial
	p.nextID++
	return cur
}

func (p *positions) unregister(id posID) {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()
	delete(p.pos, id)
	p.broadcast()
}

// writePosition handles the writer position.
type writePosition struct {
	bytesWritten int64     // Total bytes written. It's controlled by readersLock.Lock(). b.bytesWritten % len(b.buf) represents the write offset. This value monotonically increases.
	cond         sync.Cond // Used to unblock readers all at once.
}

func (w *writePosition) add(chunkSize int) {
	_ = atomic.AddInt64(&w.bytesWritten, int64(chunkSize))
	w.broadcast()
}

func (w *writePosition) broadcast() {
	w.cond.Broadcast()
}

func (w *writePosition) get() int64 {
	return atomic.LoadInt64(&w.bytesWritten)
}

func (w *writePosition) wait(bytesWritten int64) {
	w.cond.L.Lock()
	if bytesWritten == w.get() {
		w.cond.Wait()
	}
	w.cond.L.Unlock()
}

// bit is a non-resetable thread-safe bit.
type bit struct {
	bit int32
}

func (b *bit) isSet() bool {
	return atomic.LoadInt32(&b.bit) != 0
}

// set returns true if the bit was set.
func (b *bit) set() bool {
	return atomic.CompareAndSwapInt32(&b.bit, 0, 1)
}
