package password

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

// Bloom is a Bloom filter over lower-cased passwords: it says "maybe in
// the set" or "certainly not". Its file format is the magic "PFBLOOM1",
// then little-endian uint32 hash count, uint64 bit count and uint64
// entry count, then the bits.
type Bloom struct {
	K    uint32
	M    uint64 // bits
	N    uint64 // entries added
	Bits []byte
}

const bloomMagic = "PFBLOOM1"

// NewBloom makes an empty filter of m bits and k hashes.
func NewBloom(m uint64, k uint32) *Bloom {
	return &Bloom{K: k, M: m, Bits: make([]byte, (m+7)/8)}
}

func (b *Bloom) indexes(s string, f func(uint64)) {
	sum := sha256.Sum256([]byte(s))
	h1 := binary.LittleEndian.Uint64(sum[0:8])
	h2 := binary.LittleEndian.Uint64(sum[8:16]) | 1
	for i := uint64(0); i < uint64(b.K); i++ {
		f((h1 + i*h2) % b.M)
	}
}

// Add puts s in the filter.
func (b *Bloom) Add(s string) {
	b.indexes(s, func(i uint64) { b.Bits[i/8] |= 1 << (i % 8) })
	b.N++
}

// Has reports whether s may be in the filter.
func (b *Bloom) Has(s string) bool {
	in := true
	b.indexes(s, func(i uint64) {
		if b.Bits[i/8]&(1<<(i%8)) == 0 {
			in = false
		}
	})
	return in
}

// MarshalBinary writes the filter's file form.
func (b *Bloom) MarshalBinary() ([]byte, error) {
	out := make([]byte, 0, 28+len(b.Bits))
	out = append(out, bloomMagic...)
	out = binary.LittleEndian.AppendUint32(out, b.K)
	out = binary.LittleEndian.AppendUint64(out, b.M)
	out = binary.LittleEndian.AppendUint64(out, b.N)
	return append(out, b.Bits...), nil
}

// ParseBloom reads a filter's file form.
func ParseBloom(data []byte) (*Bloom, error) {
	if len(data) < 28 || string(data[:8]) != bloomMagic {
		return nil, errors.New("password: not a pi-fleet Bloom filter")
	}
	b := &Bloom{K: binary.LittleEndian.Uint32(data[8:12]), M: binary.LittleEndian.Uint64(data[12:20]), N: binary.LittleEndian.Uint64(data[20:28])}
	b.Bits = data[28:]
	if b.K == 0 || b.M == 0 || uint64(len(b.Bits)) != (b.M+7)/8 {
		return nil, errors.New("password: damaged Bloom filter")
	}
	return b, nil
}
