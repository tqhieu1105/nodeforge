package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"io"
	"sync"
)

const maxObject = 32 << 20
const maxExpanded = 128 << 20

type invalidPack struct{}

func badPack() { panic(invalidPack{}) }

type packReader struct{ data *bytes.Reader }

func (p packReader) byte() byte {
	n, e := p.data.ReadByte()
	if e != nil {
		badPack()
	}
	return n
}
func (p packReader) take(n int) []byte {
	if n < 0 || n > p.data.Len() {
		badPack()
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(p.data, b); err != nil {
		badPack()
	}
	return b
}
func (p packReader) size(n int, shift uint, more bool) int {
	for more {
		if shift > 28 {
			badPack()
		}
		b := p.byte()
		n += int(b&127) << shift
		more = b&128 != 0
		shift += 7
	}
	if n > maxObject {
		badPack()
	}
	return n
}

func applyDelta(base, delta []byte) []byte {
	p := packReader{bytes.NewReader(delta)}
	if p.size(0, 0, true) != len(base) {
		badPack()
	}
	out := make([]byte, p.size(0, 0, true))
	offset := 0
	for p.data.Len() > 0 {
		command := p.byte()
		if command&128 == 0 {
			size := int(command)
			if size == 0 || offset+size > len(out) || size > p.data.Len() {
				badPack()
			}
			if _, err := io.ReadFull(p.data, out[offset:offset+size]); err != nil {
				badPack()
			}
			offset += size
			continue
		}
		var from, size int
		for i := 0; i < 4; i++ {
			if command&(1<<i) != 0 {
				from |= int(p.byte()) << (8 * i)
			}
		}
		for i := 0; i < 3; i++ {
			if command&(16<<i) != 0 {
				size |= int(p.byte()) << (8 * i)
			}
		}
		if size == 0 {
			size = 65536
		}
		if from+size > len(base) || offset+size > len(out) {
			badPack()
		}
		copy(out[offset:offset+size], base[from:from+size])
		offset += size
	}
	if offset != len(out) {
		badPack()
	}
	return out
}

var inflaters sync.Pool

func inflate(p *bytes.Reader, n int) []byte {
	var z io.ReadCloser
	if cached := inflaters.Get(); cached != nil {
		z = cached.(io.ReadCloser)
		if z.(zlib.Resetter).Reset(p, nil) != nil {
			badPack()
		}
	} else {
		var err error
		z, err = zlib.NewReader(p)
		if err != nil {
			badPack()
		}
	}
	// Read one byte past the declared size to verify the zlib trailer and size.
	data := make([]byte, n+1)
	count, err := io.ReadFull(z, data)
	closeErr := z.Close()
	if count != n || (err != io.ErrUnexpectedEOF && err != io.EOF) || closeErr != nil {
		badPack()
	}
	inflaters.Put(z)
	return data[:n:n]
}

func unpack(data []byte, existing objectStore) (incoming objectStore, ok bool) {
	defer func() {
		if e := recover(); e != nil {
			if _, isPack := e.(invalidPack); !isPack {
				panic(e)
			}
			incoming = nil
			ok = false
		}
	}()
	if len(data) < 32 || string(data[:4]) != "PACK" {
		badPack()
	}
	version := binary.BigEndian.Uint32(data[4:8])
	count := int(binary.BigEndian.Uint32(data[8:12]))
	end := len(data) - 20
	digest := sha1.Sum(data[:end])
	if version != 2 && version != 3 || count > (end-12)/3 || !bytes.Equal(digest[:], data[end:]) {
		badPack()
	}
	p := packReader{bytes.NewReader(data[12:end])}
	incoming = make(objectStore, min(count, 4096))
	var offsets map[int]*object
	type deltaEntry struct {
		offset, baseOffset int
		base               string
		data               []byte
	}
	var pending []deltaEntry
	expanded := 0
	add := func(offset int, kind byte, data []byte) {
		expanded += len(data)
		if expanded > maxExpanded {
			badPack()
		}
		o := newObject(kind, data)
		if offsets != nil {
			offsets[offset] = o
		}
		incoming[objectID(kind, data)] = o
	}
	find := func(d deltaEntry) *object {
		if d.base == "" {
			return offsets[d.baseOffset]
		}
		if o := incoming[d.base]; o != nil {
			return o
		}
		return existing[d.base]
	}
	// Keep earlier objects by offset only when the pack actually carries deltas.
	type positioned struct {
		offset int
		object *object
	}
	positions := make([]positioned, 0, min(count, 4096))
	wireExpanded := 0
	for range count {
		start := end - p.data.Len()
		first := p.byte()
		kind := (first >> 4) & 7
		size := p.size(int(first&15), 4, first&128 != 0)
		wireExpanded += size
		if wireExpanded > maxExpanded {
			badPack()
		}
		d := deltaEntry{offset: start}
		switch kind {
		case 6:
			b := p.byte()
			distance := int(b & 127)
			for b&128 != 0 {
				b = p.byte()
				distance = (distance+1)*128 + int(b&127)
				if distance > start {
					badPack()
				}
			}
			d.baseOffset = start - distance
			if d.baseOffset < 12 || d.baseOffset >= start {
				badPack()
			}
		case 7:
			d.base = hex.EncodeToString(p.take(20))
		case commitKind, treeKind, blobKind, tagKind:
		default:
			badPack()
		}
		decoded := inflate(p.data, size)
		if kind <= tagKind {
			if offsets != nil {
				add(start, kind, decoded)
			} else {
				expanded += size
				o := newObject(kind, decoded)
				incoming[objectID(kind, decoded)] = o
				positions = append(positions, positioned{start, o})
			}
			continue
		}
		if offsets == nil {
			offsets = make(map[int]*object, len(positions))
			for _, pos := range positions {
				offsets[pos.offset] = pos.object
			}
			positions = nil
		}
		d.data = decoded
		if base := find(d); base != nil {
			add(start, base.kind, applyDelta(base.data, decoded))
		} else {
			pending = append(pending, d)
		}
	}
	if p.data.Len() != 0 {
		badPack()
	}
	for len(pending) > 0 {
		next := pending[:0]
		for _, d := range pending {
			if base := find(d); base != nil {
				add(d.offset, base.kind, applyDelta(base.data, d.data))
			} else {
				next = append(next, d)
			}
		}
		if len(next) == len(pending) {
			badPack()
		}
		pending = next
	}
	return incoming, true
}

var deflaters sync.Pool

func pack(objects []*object) []byte {
	var b bytes.Buffer
	b.WriteString("PACK")
	var header [8]byte
	binary.BigEndian.PutUint32(header[:4], 2)
	binary.BigEndian.PutUint32(header[4:], uint32(len(objects)))
	b.Write(header[:])
	var deflate *zlib.Writer
	if len(objects) > 0 {
		if cached := deflaters.Get(); cached != nil {
			deflate = cached.(*zlib.Writer)
		} else {
			deflate, _ = zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
		}
		defer deflaters.Put(deflate)
	}
	for _, o := range objects {
		n := len(o.data)
		first := o.kind<<4 | byte(n&15)
		n >>= 4
		if n != 0 {
			first |= 128
		}
		b.WriteByte(first)
		for n != 0 {
			next := byte(n & 127)
			n >>= 7
			if n != 0 {
				next |= 128
			}
			b.WriteByte(next)
		}
		deflate.Reset(&b)
		_, _ = deflate.Write(o.data)
		_ = deflate.Close()
	}
	if deflate != nil {
		deflate.Reset(io.Discard)
	}
	digest := sha1.Sum(b.Bytes())
	b.Write(digest[:])
	return b.Bytes()
}
