package xpc

import (
	"fmt"
	"reflect"
	"sync"
)

// A DataMarshaler is a value that encodes itself as XPC data, such as a
// structured value a service expects as JSON in a data entry.
// MarshalXPCData takes precedence over the value's usual conversion.
type DataMarshaler interface {
	MarshalXPCData() ([]byte, error)
}

// A DataUnmarshaler is a value that decodes itself from XPC data. data is
// valid only during the call: UnmarshalXPCData must copy what it keeps.
type DataUnmarshaler interface {
	UnmarshalXPCData(data []byte) error
}

// dataFlags record which of the data interfaces a type implements.
type dataFlags uint8

const (
	marshalsData      dataFlags = 1 << iota // T implements DataMarshaler
	ptrMarshalsData                         // *T implements DataMarshaler
	ptrUnmarshalsData                       // *T implements DataUnmarshaler
)

var (
	dataMarshalerType   = reflect.TypeFor[DataMarshaler]()
	dataUnmarshalerType = reflect.TypeFor[DataUnmarshaler]()
	dataFlagCache       sync.Map // reflect.Type -> dataFlags
)

// dataFlagsOf reports which data interfaces t implements. Encoders look
// it up once per struct field or element type, not per value.
func dataFlagsOf(t reflect.Type) dataFlags {
	if f, ok := dataFlagCache.Load(t); ok {
		return f.(dataFlags)
	}
	var f dataFlags
	// Interfaces are looked through to the dynamic value instead.
	if t.Kind() != reflect.Interface {
		if t.Implements(dataMarshalerType) {
			f |= marshalsData
		}
		pt := reflect.PointerTo(t)
		if pt.Implements(dataMarshalerType) {
			f |= ptrMarshalsData
		}
		if pt.Implements(dataUnmarshalerType) {
			f |= ptrUnmarshalsData
		}
	}
	dataFlagCache.Store(t, f)
	return f
}

// marshaled writes m as data.
func (e *encoder) marshaled(m DataMarshaler) error {
	b, err := m.MarshalXPCData()
	if err != nil {
		return fmt.Errorf("xpc: %w", err)
	}
	return e.data(b)
}

// reflectData writes rv, whose type has the data flags f.
func (e *encoder) reflectData(rv reflect.Value, f dataFlags) error {
	switch {
	case f&marshalsData != 0 && (rv.Kind() != reflect.Pointer || !rv.IsNil()):
		return e.marshaled(rv.Interface().(DataMarshaler))
	case f&ptrMarshalsData != 0 && rv.CanAddr():
		return e.marshaled(rv.Addr().Interface().(DataMarshaler))
	}
	return e.reflect(rv)
}

// unmarshalData decodes the data object whose payload is next into dv,
// which implements DataUnmarshaler through a pointer, without copying
// inline data.
func (d *decoder) unmarshalData(dv reflect.Value) error {
	n, err := d.u32()
	if err != nil {
		return err
	}
	var b []byte
	if n > maxInlineData {
		if b, err = d.attachedData(n); err != nil {
			return err
		}
	} else if b, err = d.bytes(int(n)); err != nil {
		return err
	}
	return dv.Addr().Interface().(DataUnmarshaler).UnmarshalXPCData(b)
}
