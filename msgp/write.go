package msgp

import (
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sync"
	"time"
)

func abs(i int64) int64 {
	if i < 0 {
		return -i
	}
	return i
}

// Sizer is an interface implemented
// by types that can estimate their
// size when MessagePack encoded.
// This interface is optional, but
// encoding/marshaling implementations
// may use this as a way to pre-allocate
// memory for serialization.
type Sizer interface {
	Msgsize() int
}

var (
	// Nowhere is an io.Writer to nowhere
	Nowhere io.Writer = nwhere{}

	btsType    = reflect.TypeOf(([]byte)(nil))
	writerPool = sync.Pool{
		New: func() interface{} {
			return &Writer{buf: make([]byte, 0, 1024)}
		},
	}
)

func popWriter(w io.Writer) *Writer {
	wr := writerPool.Get().(*Writer)
	wr.Reset(w)
	return wr
}

func pushWriter(wr *Writer) {
	if wr != nil && cap(wr.buf) > 256 {
		wr.w = nil
		writerPool.Put(wr)
	}
}

// FreeW frees a writer for use
// by other processes. It is not necessary
// to call FreeW on a writer. However, maintaining
// a reference to a *Writer after calling FreeW on
// it will cause undefined behavior.
func FreeW(w *Writer) { pushWriter(w) }

// Require ensures that cap(old)-len(old) >= extra
func Require(old []byte, extra int) []byte {
	if cap(old)-len(old) >= extra {
		return old
	}
	if len(old) == 0 {
		return make([]byte, 0, extra)
	}
	n := make([]byte, len(old), cap(old)-len(old)+extra)
	copy(n, old)
	return n
}

// nowhere writer
type nwhere struct{}

func (n nwhere) Write(p []byte) (int, error) { return len(p), nil }

// Marshaler is the interface implemented
// by types that know how to marshal themselves
// as MessagePack. MarshalMsg appends the marshalled
// form of the object to the provided
// byte slice, returning the extended
// slice and any errors encountered.
type Marshaler interface {
	MarshalMsg([]byte) ([]byte, error)
}

// Encodable is the interface implemented
// by types that know how to write themselves
// as MessagePack using a *msgp.Writer.
type Encodable interface {
	EncodeMsg(*Writer) error
}

// Writer is a buffered writer
// that can be used to write
// MessagePack objects to an io.Writer.
// You must call *Writer.Flush() in order
// to flush all of the buffered data
// to the underlying writer.
type Writer struct {
	w   io.Writer
	buf []byte
}

// NewWriter returns a new *Writer.
func NewWriter(w io.Writer) *Writer {
	if wr, ok := w.(*Writer); ok {
		return wr
	}
	return popWriter(w)
}

// NewWriterSize returns a writer with a custom buffer size.
func NewWriterSize(w io.Writer, sz int) *Writer {
	if sz < 16 {
		sz = 16
	}
	return &Writer{
		w:   w,
		buf: make([]byte, 0, sz),
	}
}

// Encode encodes an Encodable to an io.Writer.
func Encode(w io.Writer, e Encodable) error {
	wr := NewWriter(w)
	err := e.EncodeMsg(wr)
	if err == nil {
		err = wr.Flush()
	}
	FreeW(wr)
	return err
}

// Write writes a marshaler to an io.Writer.
func Write(w io.Writer, m Marshaler) error {
	wr := NewWriter(w)
	err := wr.Encode(m)
	if err == nil {
		err = wr.Flush()
	}
	FreeW(wr)
	return err
}

func (mw *Writer) flush() error {
	if len(mw.buf) > 0 {
		n, err := mw.w.Write(mw.buf)
		if err != nil {
			// copy unwritten data
			// back to index 0
			if n > 0 {
				mw.buf = mw.buf[:copy(mw.buf[0:], mw.buf[n:])]
			}
			return err
		}
		mw.buf = mw.buf[0:0]
		return nil
	}
	return nil
}

// Flush flushes all of the buffered
// data to the underlying writer.
func (mw *Writer) Flush() error { return mw.flush() }

// Buffered returns the number bytes in the write buffer
func (mw *Writer) Buffered() int { return len(mw.buf) }

func (mw *Writer) avail() int { return cap(mw.buf) - len(mw.buf) }

func (mw *Writer) require(n int) (int, error) {
	l := len(mw.buf)
	c := cap(mw.buf)
	if c-l >= n {
		mw.buf = mw.buf[:l+n] // grow by 'n'; return old offset
		return l, nil
	}
	err := mw.flush()
	if err != nil {
		return 0, err
	}
	// after flush,
	// len(mw.buf) = 0
	if n > c {
		mw.buf = make([]byte, n)
		return 0, nil
	}
	mw.buf = mw.buf[:n]
	return 0, nil
}

// push one byte onto the buffer
func (mw *Writer) push(b byte) error {
	l := len(mw.buf)
	if l == cap(mw.buf) {
		err := mw.flush()
		if err != nil {
			return err
		}
		l = 0
	}
	mw.buf = mw.buf[:l+1]
	mw.buf[l] = b
	return nil
}

// Write implements io.Writer, and writes
// data directly to the buffer.
func (mw *Writer) Write(p []byte) (int, error) {
	l := len(p)
	if mw.avail() >= l {
		o := len(mw.buf)
		mw.buf = mw.buf[:o+l]
		copy(mw.buf[o:], p)
		return l, nil
	}
	err := mw.flush()
	if err != nil {
		return 0, err
	}
	if l > cap(mw.buf) {
		return mw.w.Write(p)
	}
	mw.buf = mw.buf[:l]
	copy(mw.buf, p)
	return l, nil
}

// implements io.WriteString
func (mw *Writer) writeString(s string) error {
	l := len(s)

	// we have space; copy
	if mw.avail() >= l {
		o := len(mw.buf)
		mw.buf = mw.buf[:o+l]
		copy(mw.buf[o:], s)
		return nil
	}

	// we need to flush one way
	// or another.
	if err := mw.flush(); err != nil {
		return err
	}

	// shortcut: big strings go
	// straight to the underlying writer
	if l > cap(mw.buf) {
		_, err := io.WriteString(mw.w, s)
		return err
	}

	mw.buf = mw.buf[:l]
	copy(mw.buf, s)
	return nil
}

// Encode writes a Marshaler to the writer.
// Users should attempt to ensure that the
// encoded size of the marshaler is less than
// the total capacity of the buffer in order
// to avoid the writer having to re-allocate
// the entirety of the buffer.
func (mw *Writer) Encode(m Marshaler) error {
	if s, ok := m.(Sizer); ok {
		// check for available space
		sz := s.Msgsize()
		if sz > mw.avail() {
			err := mw.flush()
			if err != nil {
				return err
			}
			if sz > cap(mw.buf) {
				mw.buf = make([]byte, 0, sz)
			}
		}
	} else if mw.avail() > (cap(mw.buf) / 2) {
		// flush if we're more than half full
		if err := mw.flush(); err != nil {
			return err
		}
	}
	var err error
	old := mw.buf
	mw.buf, err = m.MarshalMsg(mw.buf)
	if err != nil {
		mw.buf = old
		return err
	}
	return nil
}

// Reset changes the underlying writer used by the Writer
func (mw *Writer) Reset(w io.Writer) {
	mw.w = w
	mw.buf = mw.buf[0:0]
}

// WriteMapHeader writes a map header of the given
// size to the writer
func (mw *Writer) WriteMapHeader(sz uint32) error {
	switch {
	case sz < 16:
		return mw.push(wfixmap(uint8(sz)))

	case sz < 1<<16-1:
		o, err := mw.require(3)
		if err != nil {
			return err
		}
		prefixu16(mw.buf[o:], mmap16, uint16(sz))
		return nil

	default:
		o, err := mw.require(5)
		if err != nil {
			return err
		}
		prefixu32(mw.buf[o:], mmap32, sz)
		return nil
	}
}

// WriteArrayHeader writes an array header of the
// given size to the writer
func (mw *Writer) WriteArrayHeader(sz uint32) error {
	switch {
	case sz < 16:
		return mw.push(wfixarray(uint8(sz)))

	case sz < math.MaxUint16:
		o, err := mw.require(3)
		if err != nil {
			return err
		}
		prefixu16(mw.buf[o:], marray16, uint16(sz))
		return nil

	default:
		o, err := mw.require(5)
		if err != nil {
			return err
		}
		prefixu32(mw.buf[o:], marray32, sz)
		return nil
	}
}

// WriteNil writes a nil byte to the buffer
func (mw *Writer) WriteNil() error {
	return mw.push(mnil)
}

// WriteFloat64 writes a float64 to the writer
func (mw *Writer) WriteFloat64(f float64) error {
	o, err := mw.require(9)
	if err != nil {
		return err
	}
	prefixu64(mw.buf[o:], mfloat64, math.Float64bits(f))
	return nil
}

// WriteFloat32 writes a float32 to the writer
func (mw *Writer) WriteFloat32(f float32) error {
	o, err := mw.require(5)
	if err != nil {
		return err
	}
	prefixu32(mw.buf[o:], mfloat32, math.Float32bits(f))
	return nil
}

// WriteInt64 writes an int64 to the writer
func (mw *Writer) WriteInt64(i int64) error {
	a := abs(i)
	switch {
	case i < 0 && i > -32:
		return mw.push(wnfixint(int8(i)))

	case i >= 0 && i < 128:
		return mw.push(wfixint(uint8(i)))

	case a < math.MaxInt8:
		o, err := mw.require(2)
		if err != nil {
			return err
		}
		mw.buf[o] = mint8
		mw.buf[o+1] = byte(int8(i))
		return nil

	case a < math.MaxInt16:
		o, err := mw.require(3)
		if err != nil {
			return err
		}
		putMint16(mw.buf[o:], int16(i))
		return nil

	case a < math.MaxInt32:
		o, err := mw.require(5)
		if err != nil {
			return err
		}
		putMint32(mw.buf[o:], int32(i))
		return nil

	default:
		o, err := mw.require(9)
		if err != nil {
			return err
		}
		putMint64(mw.buf[o:], i)
		return nil
	}

}

// WriteInt8 writes an int8 to the writer
func (mw *Writer) WriteInt8(i int8) error { return mw.WriteInt64(int64(i)) }

// WriteInt16 writes an int16 to the writer
func (mw *Writer) WriteInt16(i int16) error { return mw.WriteInt64(int64(i)) }

// WriteInt32 writes an int32 to the writer
func (mw *Writer) WriteInt32(i int32) error { return mw.WriteInt64(int64(i)) }

// WriteInt writes an int to the writer
func (mw *Writer) WriteInt(i int) error { return mw.WriteInt64(int64(i)) }

// WriteUint64 writes a uint64 to the writer
func (mw *Writer) WriteUint64(u uint64) error {
	switch {
	case u < (1 << 7):
		return mw.push(wfixint(uint8(u)))

	case u < math.MaxUint8:
		o, err := mw.require(2)
		if err != nil {
			return err
		}
		mw.buf[o] = muint8
		mw.buf[o+1] = byte(uint8(u))
		return nil
	case u < math.MaxUint16:
		o, err := mw.require(3)
		if err != nil {
			return err
		}
		putMuint16(mw.buf[o:], uint16(u))
		return nil
	case u < math.MaxUint32:
		o, err := mw.require(5)
		if err != nil {
			return err
		}
		putMuint32(mw.buf[o:], uint32(u))
		return nil
	default:
		o, err := mw.require(9)
		if err != nil {
			return err
		}
		putMuint64(mw.buf[o:], u)
		return nil
	}
}

// WriteByte is analagous to WriteUint8
func (mw *Writer) WriteByte(u byte) error { return mw.WriteUint8(uint8(u)) }

// WriteUint8 writes a uint8 to the writer
func (mw *Writer) WriteUint8(u uint8) error { return mw.WriteUint64(uint64(u)) }

// WriteUint16 writes a uint16 to the writer
func (mw *Writer) WriteUint16(u uint16) error { return mw.WriteUint64(uint64(u)) }

// WriteUint32 writes a uint32 to the writer
func (mw *Writer) WriteUint32(u uint32) error { return mw.WriteUint64(uint64(u)) }

// WriteUint writes a uint to the writer
func (mw *Writer) WriteUint(u uint) error { return mw.WriteUint64(uint64(u)) }

// WriteBytes writes binary as 'bin' to the writer
func (mw *Writer) WriteBytes(b []byte) error {
	sz := uint32(len(b))

	// write size
	switch {
	case sz < math.MaxUint8:
		o, err := mw.require(2)
		if err != nil {
			return err
		}
		mw.buf[o] = mbin8
		mw.buf[o+1] = byte(uint8(sz))

	case sz < math.MaxUint16:
		o, err := mw.require(3)
		if err != nil {
			return err
		}
		prefixu16(mw.buf[o:], mbin16, uint16(sz))

	default:
		o, err := mw.require(5)
		if err != nil {
			return err
		}
		prefixu32(mw.buf[o:], mbin32, sz)
	}

	// write body
	_, err := mw.Write(b)
	return err
}

// WriteBool writes a bool to the writer
func (mw *Writer) WriteBool(b bool) error {
	if b {
		return mw.push(mtrue)
	}
	return mw.push(mfalse)
}

// WriteString writes a messagepack string to the writer.
// (This is NOT an implementation of io.StringWriter)
func (mw *Writer) WriteString(s string) error {
	sz := uint32(len(s))

	// write size
	switch {
	case sz < 32:
		err := mw.push(wfixstr(uint8(sz)))
		if err != nil {
			return err
		}
	case sz < math.MaxUint8:
		o, err := mw.require(2)
		if err != nil {
			return err
		}
		mw.buf[o] = mstr8
		mw.buf[o+1] = byte(uint8(sz))
	case sz < math.MaxUint16:
		o, err := mw.require(3)
		if err != nil {
			return err
		}
		prefixu16(mw.buf[o:], mstr16, uint16(sz))
	default:
		o, err := mw.require(5)
		if err != nil {
			return err
		}
		prefixu32(mw.buf[o:], mstr32, sz)
	}

	// write body
	return mw.writeString(s)
}

// WriteComplex64 writes a complex64 to the writer
func (mw *Writer) WriteComplex64(f complex64) error {
	o, err := mw.require(10)
	if err != nil {
		return err
	}
	mw.buf[o] = mfixext8
	mw.buf[o+1] = Complex64Extension
	big.PutUint32(mw.buf[o+2:], math.Float32bits(real(f)))
	big.PutUint32(mw.buf[o+6:], math.Float32bits(imag(f)))
	return nil
}

// WriteComplex128 writes a complex128 to the writer
func (mw *Writer) WriteComplex128(f complex128) error {
	o, err := mw.require(18)
	if err != nil {
		return err
	}
	mw.buf[o] = mfixext16
	mw.buf[o+1] = Complex128Extension
	big.PutUint64(mw.buf[o+2:], math.Float64bits(real(f)))
	big.PutUint64(mw.buf[o+10:], math.Float64bits(imag(f)))
	return nil
}

// WriteMapStrStr writes a map[string]string to the writer
func (mw *Writer) WriteMapStrStr(mp map[string]string) (err error) {
	err = mw.WriteMapHeader(uint32(len(mp)))
	if err != nil {
		return
	}
	for key, val := range mp {
		err = mw.WriteString(key)
		if err != nil {
			return
		}
		err = mw.WriteString(val)
		if err != nil {
			return
		}
	}
	return nil
}

// WriteMapStrIntf writes a map[string]interface to the writer
func (mw *Writer) WriteMapStrIntf(mp map[string]interface{}) (err error) {
	err = mw.WriteMapHeader(uint32(len(mp)))
	if err != nil {
		return
	}
	for key, val := range mp {
		err = mw.WriteString(key)
		if err != nil {
			return
		}
		err = mw.WriteIntf(val)
		if err != nil {
			return
		}
	}
	return nil
}

// WriteIdent is a shim for e.EncodeMsg.
func (mw *Writer) WriteIdent(e Encodable) error {
	return e.EncodeMsg(mw)
}

// WriteTime writes a time.Time object to the wire.
//
// Time is encoded as Unix time, which means that
// location (time zone) data is removed from the object.
// The encoded object itself is 12 bytes: 8 bytes for
// a big-endian 64-bit integer denoting seconds
// elapsed since "zero" Unix time, followed by 4 bytes
// for a big-endian 32-bit signed integer denoting
// the nanosecond offset of the time. This encoding
// is intended to ease portability accross languages.
// (Note that this is *not* the standard time.Time
// binary encoding, because its implementation relies
// heavily on the internal representation used by the
// time package.)
func (mw *Writer) WriteTime(t time.Time) error {
	t = t.UTC()
	o, err := mw.require(15)
	if err != nil {
		return err
	}
	mw.buf[o] = mext8
	mw.buf[o+1] = 12
	mw.buf[o+2] = TimeExtension
	putUnix(mw.buf[o+3:], t.Unix(), int32(t.Nanosecond()))
	return nil
}

// WriteIntf writes the concrete type of 'v'.
// WriteIntf will error if 'v' is not one of the following:
//  - A bool, float, string, []byte, int, uint, or complex
//  - A map of supported types (with string keys)
//  - An array or slice of supported types
//  - A pointer to a supported type
//  - A type that satisfies the msgp.Encodable interface
//  - A type that satisfies the msgp.Extension interface
func (mw *Writer) WriteIntf(v interface{}) error {
	if v == nil {
		return mw.WriteNil()
	}
	switch v := v.(type) {

	// preferred interfaces

	case Encodable:
		return v.EncodeMsg(mw)
	case Marshaler:
		return mw.Encode(v)
	case Extension:
		return mw.WriteExtension(v)

	// concrete types

	case bool:
		return mw.WriteBool(v)
	case float32:
		return mw.WriteFloat32(v)
	case float64:
		return mw.WriteFloat64(v)
	case complex64:
		return mw.WriteComplex64(v)
	case complex128:
		return mw.WriteComplex128(v)
	case uint8:
		return mw.WriteUint8(v)
	case uint16:
		return mw.WriteUint16(v)
	case uint32:
		return mw.WriteUint32(v)
	case uint64:
		return mw.WriteUint64(v)
	case uint:
		return mw.WriteUint(v)
	case int8:
		return mw.WriteInt8(v)
	case int16:
		return mw.WriteInt16(v)
	case int32:
		return mw.WriteInt32(v)
	case int64:
		return mw.WriteInt64(v)
	case int:
		return mw.WriteInt(v)
	case string:
		return mw.WriteString(v)
	case []byte:
		return mw.WriteBytes(v)
	case map[string]string:
		return mw.WriteMapStrStr(v)
	case map[string]interface{}:
		return mw.WriteMapStrIntf(v)
	case time.Time:
		return mw.WriteTime(v)
	}

	val := reflect.ValueOf(v)
	if !isSupported(val.Kind()) || !val.IsValid() {
		return fmt.Errorf("msgp: type %s not supported", val)
	}

	switch val.Kind() {
	case reflect.Ptr:
		if val.IsNil() {
			return mw.WriteNil()
		}
		return mw.WriteIntf(val.Elem().Interface())
	case reflect.Slice:
		return mw.writeSlice(val)
	case reflect.Map:
		return mw.writeMap(val)
	}
	return &ErrUnsupportedType{val.Type()}
}

func (mw *Writer) writeMap(v reflect.Value) (err error) {
	if v.Elem().Kind() != reflect.String {
		return errors.New("msgp: map keys must be strings")
	}
	ks := v.MapKeys()
	err = mw.WriteMapHeader(uint32(len(ks)))
	if err != nil {
		return
	}
	for _, key := range ks {
		val := v.MapIndex(key)
		err = mw.WriteString(key.String())
		if err != nil {
			return
		}
		err = mw.WriteIntf(val.Interface())
		if err != nil {
			return
		}
	}
	return
}

func (mw *Writer) writeSlice(v reflect.Value) (err error) {
	// is []byte
	if v.Type().ConvertibleTo(btsType) {
		return mw.WriteBytes(v.Bytes())
	}

	sz := uint32(v.Len())
	err = mw.WriteArrayHeader(sz)
	if err != nil {
		return
	}
	for i := uint32(0); i < sz; i++ {
		err = mw.WriteIntf(v.Index(int(i)).Interface())
		if err != nil {
			return
		}
	}
	return
}

func (mw *Writer) writeStruct(v reflect.Value) error {
	if enc, ok := v.Interface().(Encodable); ok {
		return enc.EncodeMsg(mw)
	}
	if mar, ok := v.Interface().(Marshaler); ok {
		return mw.Encode(mar)
	}
	return fmt.Errorf("msgp: unsupported type: %s", v.Type())
}

func (mw *Writer) writeVal(v reflect.Value) error {
	if !isSupported(v.Kind()) {
		return fmt.Errorf("msgp: msgp/enc: type %q not supported", v.Type())
	}

	// shortcut for nil values
	if v.IsNil() {
		return mw.WriteNil()
	}
	switch v.Kind() {
	case reflect.Bool:
		return mw.WriteBool(v.Bool())

	case reflect.Float32, reflect.Float64:
		return mw.WriteFloat64(v.Float())

	case reflect.Complex64, reflect.Complex128:
		return mw.WriteComplex128(v.Complex())

	case reflect.Int, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int8:
		return mw.WriteInt64(v.Int())

	case reflect.Interface, reflect.Ptr:
		if v.IsNil() {
			mw.WriteNil()
		}
		return mw.writeVal(v.Elem())

	case reflect.Map:
		return mw.writeMap(v)

	case reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint8:
		return mw.WriteUint64(v.Uint())

	case reflect.String:
		return mw.WriteString(v.String())

	case reflect.Slice, reflect.Array:
		return mw.writeSlice(v)

	case reflect.Struct:
		return mw.writeStruct(v)

	}
	return fmt.Errorf("msgp: msgp/enc: type %q not supported", v.Type())
}

// is the reflect.Kind encodable?
func isSupported(k reflect.Kind) bool {
	switch k {
	case reflect.Func, reflect.Chan, reflect.Invalid, reflect.UnsafePointer:
		return false
	default:
		return true
	}
}

// GuessSize guesses the size of the underlying
// value of 'i'. If the underlying value is not
// a simple builtin (or []byte), GuessSize defaults
// to 512.
func GuessSize(i interface{}) int {
	if s, ok := i.(Sizer); ok {
		return s.Msgsize()
	} else if e, ok := i.(Extension); ok {
		return ExtensionPrefixSize + e.Len()
	} else if i == nil {
		return NilSize
	}

	switch i := i.(type) {
	case float64:
		return Float64Size
	case float32:
		return Float32Size
	case uint8, uint16, uint32, uint64, uint:
		return UintSize
	case int8, int16, int32, int64, int:
		return IntSize
	case []byte:
		return BytesPrefixSize + len(i)
	case string:
		return StringPrefixSize + len(i)
	case complex64:
		return Complex64Size
	case complex128:
		return Complex128Size
	case bool:
		return BoolSize
	case map[string]interface{}:
		s := MapHeaderSize
		for key, val := range i {
			s += StringPrefixSize + len(key) + GuessSize(val)
		}
		return s
	case map[string]string:
		s := MapHeaderSize
		for key, val := range i {
			s += 2*StringPrefixSize + len(key) + len(val)
		}
		return s
	default:
		return 512
	}
}
