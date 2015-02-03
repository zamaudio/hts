// Copyright ©2012 The bíogo.bam Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bgzf

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"io"
)

// Reader implements BGZF blocked gzip decompression.
type Reader struct {
	gzip.Header
	r io.Reader

	// lastChunk is the virtual file offset
	// interval of the last successful read
	// or seek operation.
	lastChunk Chunk

	// nextBase is the file offset of the
	// block following the current block.
	nextBase int64

	active *decompressor

	// Cache is the Reader block cache. If Cache is not nil,
	// the cache is queried for blocks before an attempt to
	// read from the underlying io.Reader.
	Cache Cache

	err error
}

type decompressor struct {
	owner *Reader

	gz gzip.Reader

	// Underlying Reader.
	r flate.Reader

	// Positions within underlying data stream
	offset int64 // Current offset in stream - possibly virtual.
	mark   int64 // Offset at start of useUnderlying.

	// Buffered compressed data from read ahead.
	i   int // Current position in buffered data.
	n   int // Total size of buffered data.
	buf [MaxBlockSize]byte

	// Decompressed data.
	decompressed Block

	err error
}

func makeReader(r io.Reader) flate.Reader {
	switch r := r.(type) {
	case *decompressor:
		panic("bgzf: illegal use of internal type")
	case flate.Reader:
		return r
	default:
		return bufio.NewReader(r)
	}
}

func newDecompressor() *decompressor { return &decompressor{} }

func (d *decompressor) init(r io.Reader) (*decompressor, error) {
	d.r = makeReader(r)
	d.err = d.gz.Reset(d)
	if d.err != nil {
		return d, d.err
	}
	if expectedBlockSize(d.gz.Header) < 0 {
		d.err = ErrNoBlockSize
	}
	return d, d.err
}

// lazyBlock conditionally creates a ready to use Block and returns whether
// the Block subsequently held by the decompressor needs to be reset before
// being filled.
func (d *decompressor) lazyBlock() bool {
	if d.decompressed == nil {
		if w, ok := d.owner.Cache.(Wrapper); ok {
			d.decompressed = w.Wrap(&block{owner: d.owner})
		} else {
			d.decompressed = &block{owner: d.owner}
		}
		return false
	}
	if !d.decompressed.ownedBy(d.owner) {
		d.decompressed.setOwner(d.owner)
	}
	return true
}

func (d *decompressor) header() gzip.Header {
	return d.gz.Header
}

func (d *decompressor) isBuffered() bool { return d.n != 0 }

// Read provides the Read method for the decompressor's gzip.Reader.
func (d *decompressor) Read(p []byte) (int, error) {
	var (
		n   int
		err error
	)
	if d.isBuffered() {
		if d.i >= d.n {
			return 0, io.EOF
		}
		if n := d.n - d.i; len(p) > n {
			p = p[:n]
		}
		n = copy(p, d.buf[d.i:])
		d.i += n
	} else {
		n, err = d.r.Read(p)
	}
	d.offset += int64(n)
	return n, err
}

// ReadByte provides the ReadByte method for the decompressor's gzip.Reader.
func (d *decompressor) ReadByte() (byte, error) {
	var (
		b   byte
		err error
	)
	if d.isBuffered() {
		if d.i == d.n {
			return 0, io.EOF
		}
		b = d.buf[d.i]
		d.i++
	} else {
		b, err = d.r.ReadByte()
	}
	if err == nil {
		d.offset++
	}
	return b, err
}

func (d *decompressor) reset() {
	needReset := d.lazyBlock()

	if d.gotBlockFor(d.owner.nextBase) {
		return
	}

	if needReset && d.offset != d.owner.nextBase {
		// It should not be possible for the expected next block base
		// to be out of register with the count reader unless Seek
		// has been called, so we know the base reader must be an
		// io.ReadSeeker.
		d.err = d.seek(d.owner.r.(io.ReadSeeker), d.owner.nextBase)
		if d.err != nil {
			return
		}
	}

	d.err = d.fill(needReset)
}

func (d *decompressor) seekRead(r io.ReadSeeker, off int64) {
	d.lazyBlock()

	if off == d.decompressed.Base() && d.decompressed.hasData() {
		return
	}

	if d.gotBlockFor(off) {
		return
	}

	d.err = d.seek(r, off)
	if d.err != nil {
		return
	}

	d.err = d.fill(true)
}

func (d *decompressor) seek(r io.ReadSeeker, off int64) error {
	_, err := r.Seek(off, 0)
	if err != nil {
		return err
	}

	type reseter interface {
		Reset(io.Reader)
	}
	switch cr := d.r.(type) {
	case reseter:
		cr.Reset(r)
	default:
		d.r = makeReader(r)
	}
	d.offset = off

	return nil
}

// gotBlockFor returns true if the decompressor has access to a cache
// and that cache holds the block with given base and the correct
// owner, otherwise it returns false.
// gotBlockFor has side effects of recovering the block and putting
// the currently active block into the cache. If the cache returns
// a block owned by another reader, it is discarded.
func (d *decompressor) gotBlockFor(base int64) bool {
	if d.owner.Cache != nil {
		dec := d.decompressed
		if blk := d.owner.Cache.Get(base); blk != nil && blk.ownedBy(d.owner) {
			if dec != nil && dec.hasData() {
				// TODO(kortschak): Under some conditions, e.g. FIFO
				// cache we will be discarding a non-nil evicted Block.
				// Consider retaining these in a sync.Pool.
				d.owner.Cache.Put(dec)
			}
			if d.err = blk.seek(0); d.err == nil {
				d.decompressed = blk
				d.owner.nextBase = blk.nextBase()
				return true
			}
		}
		if dec != nil && dec.hasData() {
			dec, retained := d.owner.Cache.Put(dec)
			if retained {
				d.decompressed = dec
				d.lazyBlock()
			}
		}
	}

	return false
}

func (d *decompressor) useUnderlying() { d.n = 0; d.mark = d.offset }

func (d *decompressor) readAhead(n int) error {
	d.i, d.n = 0, n
	var err error
	lr := io.LimitedReader{R: d.r, N: int64(n)}
	for i, _n := 0, 0; i < n && err == nil; i += _n {
		_n, err = lr.Read(d.buf[i:])
		if err != nil {
			break
		}
	}
	return err
}

func (d *decompressor) deltaOffset() int64 { return d.offset - d.mark }

func (d *decompressor) fill(reset bool) error {
	dec := d.decompressed

	if reset {
		dec.setBase(d.offset)

		d.useUnderlying()
		err := d.gz.Reset(d)
		bs := expectedBlockSize(d.gz.Header)
		if err == nil && bs < 0 {
			err = ErrNoBlockSize
		}
		if err != nil {
			return err
		}
		err = d.readAhead(bs - int(d.deltaOffset()))
		if err != nil {
			return err
		}
	}

	dec.setHeader(d.gz.Header)
	d.gz.Multistream(false)
	return dec.readFrom(&d.gz)
}

func expectedBlockSize(h gzip.Header) int {
	i := bytes.Index(h.Extra, bgzfExtraPrefix)
	if i < 0 || i+5 >= len(h.Extra) {
		return -1
	}
	return (int(h.Extra[i+4]) | int(h.Extra[i+5])<<8) + 1
}

// NewReader returns a new BGZF reader.
//
// The number of concurrent read decompressors is specified by
// rd (currently ignored).
func NewReader(r io.Reader, rd int) (*Reader, error) {
	d, err := newDecompressor().init(r)
	if err != nil {
		return nil, err
	}
	bg := &Reader{
		Header: d.header(),
		r:      r,
		active: d,
	}
	d.owner = bg
	return bg, nil
}

// Offset is a BGZF virtual offset.
type Offset struct {
	File  int64
	Block uint16
}

// Chunk is a region of a BGZF file.
type Chunk struct {
	Begin Offset
	End   Offset
}

// Seek performs a seek operation to the given virtual offset.
func (bg *Reader) Seek(off Offset) error {
	rs, ok := bg.r.(io.ReadSeeker)
	if !ok {
		return ErrNotASeeker
	}

	bg.active.seekRead(rs, off.File)
	bg.err = bg.active.err
	if bg.err != nil {
		return bg.err
	}
	bg.Header = bg.active.header()
	bg.nextBase = bg.active.decompressed.nextBase()

	bg.err = bg.active.decompressed.seek(int64(off.Block))
	if bg.err == nil {
		bg.lastChunk = Chunk{Begin: off, End: off}
	}

	return bg.err
}

// LastChunk returns the region of the BGZF file read by the last read
// operation or the resulting virtual offset of the last successful
// seek operation.
func (bg *Reader) LastChunk() Chunk { return bg.lastChunk }

// Close closes the reader and releases resources.
func (bg *Reader) Close() error {
	bg.Cache = nil
	return bg.active.gz.Close()
}

// Read implements the io.Reader interface.
func (bg *Reader) Read(p []byte) (int, error) {
	if bg.err != nil {
		return 0, bg.err
	}

	dec := bg.active.decompressed
	if dec != nil {
		dec.beginTx()
	} else {
		bs := expectedBlockSize(bg.Header)
		bg.err = bg.active.readAhead(bs - int(bg.active.deltaOffset()))
		if bg.err != nil {
			return 0, bg.err
		}
	}

	if dec == nil || dec.len() == 0 {
		dec, bg.err = bg.resetDecompressor()
		if bg.err != nil {
			return 0, bg.err
		}
	}

	var n int
	for n < len(p) && bg.err == nil {
		var _n int
		_n, bg.err = dec.Read(p[n:])
		if _n > 0 {
			bg.lastChunk = dec.endTx()
		}
		n += _n
		if bg.err == io.EOF {
			if n == len(p) {
				bg.err = nil
				break
			}

			dec, bg.err = bg.resetDecompressor()
			if bg.err != nil {
				break
			}
		}
	}

	return n, bg.err
}

func (bg *Reader) resetDecompressor() (Block, error) {
	bg.active.reset()
	if bg.active.err != nil {
		return nil, bg.active.err
	}
	bg.Header = bg.active.header()
	bg.nextBase = bg.active.decompressed.nextBase()
	return bg.active.decompressed, nil
}
