// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package codec implements the general-purpose part of an encoder for Go
// values. It relies on code generation rather than reflection so it is
// significantly faster than reflection-based encoders like gob. It also
// preserves sharing among struct pointers (but not other forms of sharing, like
// sub-slices). These features are sufficient for encoding the structures of the
// go/ast package, which is its sole purpose.
package codec

import (
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"runtime"
)

// An Encoder encodes Go values into a sequence of bytes.
// To use an Encoder:
// - Create one with NewEncoder.
// - Call the Encode method one or more times.
// - Retrieve the resulting bytes by calling Bytes.
type Encoder struct {
	buf      []byte
	typeNums map[reflect.Type]int
	seen     map[interface{}]uint64 // for references; see StartStruct
}

// NewEncoder returns an Encoder.
func NewEncoder() *Encoder {
	return &Encoder{
		typeNums: map[reflect.Type]int{},
		seen:     map[interface{}]uint64{},
	}
}

// Encode encodes x.
func (e *Encoder) Encode(x interface{}) (err error) {
	defer func() { handlePanic(&err, recover()) }()
	e.EncodeAny(x)
	return nil
}

func handlePanic(errp *error, recovered interface{}) {
	if recovered == nil {
		// No panic; do nothing.
		return
	}
	// If the panic is from the Go runtime, re-panic.
	if _, ok := recovered.(runtime.Error); ok {
		panic(recovered)
	}
	// If the panic value is any other error, set errp
	// and return normally.
	if err, ok := recovered.(error); ok {
		*errp = err
		return
	}
	// Panic on anything else.
	panic(recovered)
}

func (e *Encoder) fail(err error) {
	panic(err)
}

func (e *Encoder) failf(format string, args ...interface{}) {
	e.fail(fmt.Errorf(format, args...))
}

// Bytes returns the encoded byte slice.
func (e *Encoder) Bytes() []byte {
	data := e.buf                 // remember the data
	e.buf = nil                   // start with a fresh buffer
	e.encodeInitial()             // encode metadata
	return append(e.buf, data...) // concatenate metadata and data
}

// A Decoder decodes a Go value encoded by an Encoder.
// To use a Decoder:
// - Pass NewDecoder the return value of Encoder.Bytes.
// - Call the Decode method once for each call to Encoder.Encode.
type Decoder struct {
	buf       []byte
	i         int
	typeInfos []*typeInfo
	refs      []interface{} // list of struct pointers, in the order seen
}

// NewDecoder returns a Decoder for the given bytes.
func NewDecoder(data []byte) *Decoder {
	return &Decoder{buf: data, i: 0}
}

// Decode decodes a value encoded with Encoder.Encode.
func (d *Decoder) Decode() (_ interface{}, err error) {
	defer func() { handlePanic(&err, recover()) }()
	if d.typeInfos == nil {
		d.decodeInitial()
	}
	return d.DecodeAny(), nil
}

func (d *Decoder) fail(err error) {
	panic(err)
}

func (d *Decoder) failf(format string, args ...interface{}) {
	d.fail(fmt.Errorf(format, args...))
}

func (d *Decoder) badcode(c byte) {
	d.failf("bad code: %d", c)
}

//////////////// Reading From and Writing To the Buffer

func (e *Encoder) writeByte(b byte) {
	e.buf = append(e.buf, b)
}

func (d *Decoder) readByte() byte {
	b := d.curByte()
	d.i++
	return b
}

// curByte returns the next byte to be read
// without actually consuming it.
func (d *Decoder) curByte() byte {
	return d.buf[d.i]
}

func (e *Encoder) writeBytes(b []byte) {
	e.buf = append(e.buf, b...)
}

// readBytes reads and returns the given number of bytes.
// It fails if there are not enough bytes in the input.
func (d *Decoder) readBytes(n int) []byte {
	d.i += n
	return d.buf[d.i-n : d.i]
}

func (e *Encoder) writeString(s string) {
	e.buf = append(e.buf, s...)
}

func (d *Decoder) readString(len int) string {
	return string(d.readBytes(len))
}

func (e *Encoder) writeUint32(u uint32) {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], u)
	e.writeBytes(buf[:])
}

func (d *Decoder) readUint32() uint32 {
	return binary.LittleEndian.Uint32(d.readBytes(4))
}

func (e *Encoder) writeUint64(u uint64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], u)
	e.writeBytes(buf[:])
}

func (d *Decoder) readUint64() uint64 {
	return binary.LittleEndian.Uint64(d.readBytes(8))
}

//////////////// Encoding Scheme

// Every encoded value begins with a single byte that describes what (if
// anything) follows. There is enough information to skip over the value, since
// the decoder must be able to do that if it encounters a struct field it
// doesn't know.
//
// Most of the values of that initial byte can be devoted to small unsigned
// integers. For example, the number 17 is represented by the single byte 17.
// Only a few byte values have special meaning.
//
// The nil code indicates that the value is nil. We don't absolutely need this:
// we could always represent the nil value for a type as something that couldn't
// be mistaken for an encoded value of that type. For instance, we could use 0
// for nil in the case of slices (which always begin with the nValues code), and
// for pointers to numbers like *int, we could use something like "nBytes 0".
// But it is simpler to have a reserved value for nil.

// The nBytes code indicates that an unsigned integer N is encoded next,
// followed by N bytes of data. This is used to represent strings and byte
// slices, as well numbers bigger than can fit into the initial byte. For
// example, the string "hi" is represented as:
//   nBytes 2 'h' 'i'
//
// Unsigned integers that can't fit into the initial byte are encoded as byte
// sequences of length 4 or 8, holding little-endian uint32 or uint64 values. We
// use uint32s where possible to save space. We could have saved more space by
// also considering 16-byte numbers, or using a variable-length encoding like
// varints or gob's representation, but it didn't seem worth the additional
// complexity.
//
// The nValues code is for sequences of values whose size is known beforehand,
// like a Go slice or array. The slice []string{"hi", "bye"} is encoded as
//   nValues 2 nBytes 2 'h' 'i' nBytes 3 'b' 'y' 'e'
//
// The ref code is used to refer to an earlier encoded value. It is followed by
// a uint denoting the index data of the value to use.
//
// The start and end codes delimit a value whose length is unknown beforehand.
// It is used for structs.
const (
	nilCode = 255 - iota // a nil value
	// reserve a few values for future use
	reserved1
	reserved2
	reserved3
	reserved4
	reserved5
	reserved6
	reserved7
	nBytesCode  // uint n follows, then n bytes
	nValuesCode // uint n follows, then n values
	refCode     // uint n follows, referring to a previous value
	startCode   // start of a value of indeterminate length
	endCode     // end of a value that began with start
	// Bytes less than endCode represent themselves.
)

// EncodeUint encodes a uint64.
func (e *Encoder) EncodeUint(u uint64) {
	switch {
	case u < endCode:
		// u fits into the initial byte.
		e.writeByte(byte(u))
	case u <= math.MaxUint32:
		// Encode as a sequence of 4 bytes, the little-endian representation of
		// a uint32.
		e.writeByte(nBytesCode)
		e.writeByte(4)
		e.writeUint32(uint32(u))
	default:
		// Encode as a sequence of 8 bytes, the little-endian representation of
		// a uint64.
		e.writeByte(nBytesCode)
		e.writeByte(8)
		e.writeUint64(u)
	}
}

// DecodeUint decodes a uint64.
func (d *Decoder) DecodeUint() uint64 {
	b := d.readByte()
	switch {
	case b < endCode:
		return uint64(b)
	case b == nBytesCode:
		switch n := d.readByte(); n {
		case 4:
			return uint64(d.readUint32())
		case 8:
			return d.readUint64()
		default:
			d.failf("DecodeUint: bad length %d", n)
		}
	default:
		d.badcode(b)
	}
	return 0
}

// EncodeInt encodes a signed integer.
func (e *Encoder) EncodeInt(i int64) {
	// Encode small negative as well as small positive integers efficiently.
	// Algorithm from gob; see "Encoding Details" at https://pkg.go.dev/encoding/gob.
	var u uint64
	if i < 0 {
		u = (^uint64(i) << 1) | 1 // complement i, bit 0 is 1
	} else {
		u = (uint64(i) << 1) // do not complement i, bit 0 is 0
	}
	e.EncodeUint(u)
}

// DecodeInt decodes a signed integer.
func (d *Decoder) DecodeInt() int64 {
	u := d.DecodeUint()
	if u&1 == 1 {
		return int64(^(u >> 1))
	}
	return int64(u >> 1)
}

// encodeLen encodes the length of a byte sequence.
func (e *Encoder) encodeLen(n int) {
	e.writeByte(nBytesCode)
	e.EncodeUint(uint64(n))
}

// decodeLen decodes the length of a byte sequence.
func (d *Decoder) decodeLen() int {
	if b := d.readByte(); b != nBytesCode {
		d.badcode(b)
	}
	return int(d.DecodeUint())
}

// EncodeBytes encodes a byte slice.
func (e *Encoder) EncodeBytes(b []byte) {
	e.encodeLen(len(b))
	e.writeBytes(b)
}

// DecodeBytes decodes a byte slice.
// It does no copying.
func (d *Decoder) DecodeBytes() []byte {
	return d.readBytes(d.decodeLen())
}

// EncodeString encodes a string.
func (e *Encoder) EncodeString(s string) {
	e.encodeLen(len(s))
	e.writeString(s)
}

// DecodeString decodes a string.
func (d *Decoder) DecodeString() string {
	return d.readString(d.decodeLen())
}

// EncodeBool encodes a bool.
func (e *Encoder) EncodeBool(b bool) {
	if b {
		e.writeByte(1)
	} else {
		e.writeByte(0)
	}
}

// DecodeBool decodes a bool.
func (d *Decoder) DecodeBool() bool {
	b := d.readByte()
	switch b {
	case 0:
		return false
	case 1:
		return true
	default:
		d.failf("bad bool: %d", b)
		return false
	}
}

// EncodeFloat encodes a float64.
func (e *Encoder) EncodeFloat(f float64) {
	e.EncodeUint(math.Float64bits(f))
}

// DecodeFloat decodes a float64.
func (d *Decoder) DecodeFloat() float64 {
	return math.Float64frombits(d.DecodeUint())
}

func (e *Encoder) EncodeNil() {
	e.writeByte(nilCode)
}

// StartList should be called before encoding any sequence of variable-length
// values.
func (e *Encoder) StartList(len int) {
	e.writeByte(nValuesCode)
	e.EncodeUint(uint64(len))
}

// StartList should be called before decoding any sequence of variable-length
// values. It returns -1 if the encoded list was nil. Otherwise, it returns the
// length of the sequence.
func (d *Decoder) StartList() int {
	switch b := d.readByte(); b {
	case nilCode:
		return -1
	case nValuesCode:
		return int(d.DecodeUint())
	default:
		d.badcode(b)
		return 0
	}
}

//////////////// Struct Support

// StartStruct should be called before encoding a struct pointer. The isNil
// argument says whether the pointer is nil. The p argument is the struct
// pointer. If StartStruct returns false, encoding should not proceed.
func (e *Encoder) StartStruct(isNil bool, p interface{}) bool {
	if isNil {
		e.EncodeNil()
		return false
	}
	if u, ok := e.seen[p]; ok {
		// If we have already seen this struct pointer,
		// encode a reference to it.
		e.writeByte(refCode)
		e.EncodeUint(u)
		return false // Caller should not encode the struct.
	}
	// Note that we have seen this pointer, and assign it
	// its position in the encoding.
	e.seen[p] = uint64(len(e.seen))
	e.writeByte(startCode)
	return true
}

// StartStruct should be called before decoding a struct pointer. If it returns
// false, decoding should not proceed. If it returns true and the second return
// value is non-nil, it is a reference to a previous value and should be used
// instead of proceeding with decoding.
func (d *Decoder) StartStruct() (bool, interface{}) {
	b := d.readByte()
	switch b {
	case nilCode: // do not set the pointer
		return false, nil
	case refCode:
		u := d.DecodeUint()
		return true, d.refs[u]
	case startCode:
		return true, nil
	default:
		d.badcode(b)
		return false, nil // unreached, needed for compiler
	}
}

// StoreRef should be called by a struct decoder immediately after it allocates
// a struct pointer.
func (d *Decoder) StoreRef(p interface{}) {
	d.refs = append(d.refs, p)
}

// EndStruct should be called after encoding a struct.
func (e *Encoder) EndStruct() {
	e.writeByte(endCode)
}

// NextStructField should be called by a struct decoder in a loop.
// It returns the field number of the next encoded field, or -1
// if there are no more fields.
func (d *Decoder) NextStructField() int {
	if d.curByte() == endCode {
		d.readByte() // consume the end byte
		return -1
	}
	return int(d.DecodeUint())
}

// UnknownField should be called by a struct decoder
// when it sees a field number that it doesn't know.
func (d *Decoder) UnknownField(typeName string, num int) {
	d.skip()
}

// skip reads past a value in the input.
func (d *Decoder) skip() {
	b := d.readByte()
	if b < endCode {
		// Small integers represent themselves in a single byte.
		return
	}
	switch b {
	case nilCode:
		// Nothing follows.
	case nBytesCode:
		// A uint n and n bytes follow. It is efficient to call readBytes here
		// because it does no allocation.
		d.readBytes(int(d.DecodeUint()))
	case nValuesCode:
		// A uint n and n values follow.
		n := int(d.DecodeUint())
		for i := 0; i < n; i++ {
			d.skip()
		}
	case refCode:
		// A uint follows.
		d.DecodeUint()
	case startCode:
		// Skip until we see endCode.
		for d.curByte() != endCode {
			d.skip()
		}
		d.readByte() // consume the endCode byte
	default:
		d.badcode(b)
	}
}

//////////////// Encoding Arbitrary Values

// EncodeAny encodes a Go type. The type must have
// been registered with Register.
func (e *Encoder) EncodeAny(x interface{}) {
	// Encode a nil interface value with a zero.
	if x == nil {
		e.writeByte(0)
		return
	}
	// Find the typeInfo for the type, which has the encoder.
	t := reflect.TypeOf(x)
	ti := typeInfosByType[t]
	if ti == nil {
		e.failf("unregistered type %s", t)
	}
	// Assign a number to the type if we haven't already.
	num, ok := e.typeNums[t]
	if !ok {
		num = len(e.typeNums)
		e.typeNums[t] = num
	}
	// Encode a pair (2-element list) of the type number and the encoded value.
	e.StartList(2)
	e.EncodeUint(uint64(num))
	ti.encode(e, x)
}

// DecodeAny decodes a value encoded by EncodeAny.
func (d *Decoder) DecodeAny() interface{} {
	// If we're looking at a zero, this is a nil interface.
	if d.curByte() == 0 {
		d.readByte() // consume the byte
		return nil
	}
	// Otherwise, we should have a two-item list: type number and value.
	n := d.StartList()
	if n != 2 {
		d.failf("DecodeAny: bad list length %d", n)
	}
	num := d.DecodeUint()
	if num >= uint64(len(d.typeInfos)) {
		d.failf("type number %d out of range", num)
	}
	ti := d.typeInfos[num]
	return ti.decode(d)
}

// encodeInitial encodes metadata that appears at the start of the
// encoded byte slice.
func (e *Encoder) encodeInitial() {
	// Encode the list of type names we saw, in the order we
	// assigned numbers to them.
	names := make([]string, len(e.typeNums))
	for t, num := range e.typeNums {
		names[num] = typeName(t)
	}
	e.StartList(len(names))
	for _, n := range names {
		e.EncodeString(n)
	}
}

// decodeInitial decodes metadata that appears at the start of the
// encoded byte slice.
func (d *Decoder) decodeInitial() {
	// Decode the list of type names. The number of a type is its position in
	// the list.
	n := d.StartList()
	d.typeInfos = make([]*typeInfo, n)
	for num := 0; num < n; num++ {
		name := d.DecodeString()
		ti := typeInfosByName[name]
		if ti == nil {
			d.failf("unregistered type: %s", name)
		}
		d.typeInfos[num] = ti
	}
}

//////////////// Type Registry

// All types subject to encoding must be registered, even
// builtin types.

// A typeInfo describes how to encode and decode a type.
type typeInfo struct {
	name   string // e.g. "go/ast.File"
	encode encodeFunc
	decode decodeFunc
}

type (
	encodeFunc func(*Encoder, interface{})
	decodeFunc func(*Decoder) interface{}
)

var (
	typeInfosByName = map[string]*typeInfo{}
	typeInfosByType = map[reflect.Type]*typeInfo{}
)

// Register records the type of x for use by Encoders and Decoders.
func Register(x interface{}, enc encodeFunc, dec decodeFunc) {
	t := reflect.TypeOf(x)
	tn := typeName(t)
	if _, ok := typeInfosByName[tn]; ok {
		panic(fmt.Sprintf("codec.Register: duplicate type %s (typeName=%q)", t, tn))
	}
	ti := &typeInfo{
		name:   tn,
		encode: enc,
		decode: dec,
	}
	typeInfosByName[ti.name] = ti
	typeInfosByType[t] = ti
}

// typeName returns the full, qualified name for a type.
func typeName(t reflect.Type) string {
	if t.PkgPath() == "" {
		return t.String()
	}
	return t.PkgPath() + "." + t.Name()
}

var builtinTypes []reflect.Type

func init() {
	Register(int64(0),
		func(e *Encoder, x interface{}) { e.EncodeInt(x.(int64)) },
		func(d *Decoder) interface{} { return d.DecodeInt() })
	Register(uint64(0),
		func(e *Encoder, x interface{}) { e.EncodeUint(x.(uint64)) },
		func(d *Decoder) interface{} { return d.DecodeUint() })
	Register(int(0),
		func(e *Encoder, x interface{}) { e.EncodeInt(int64(x.(int))) },
		func(d *Decoder) interface{} { return int(d.DecodeInt()) })
	Register(float64(0),
		func(e *Encoder, x interface{}) { e.EncodeFloat(x.(float64)) },
		func(d *Decoder) interface{} { return d.DecodeFloat() })
	Register(false,
		func(e *Encoder, x interface{}) { e.EncodeBool(x.(bool)) },
		func(d *Decoder) interface{} { return d.DecodeBool() })
	Register("",
		func(e *Encoder, x interface{}) { e.EncodeString(x.(string)) },
		func(d *Decoder) interface{} { return d.DecodeString() })
	Register([]byte(nil),
		func(e *Encoder, x interface{}) { e.EncodeBytes(x.([]byte)) },
		func(d *Decoder) interface{} { return d.DecodeBytes() })

	for t := range typeInfosByType {
		builtinTypes = append(builtinTypes, t)
	}
}
