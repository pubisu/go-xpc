package xpc

import (
	"fmt"
	"math"
	"reflect"
	"strings"
)

func overflow(src any, dv reflect.Value) error {
	return fmt.Errorf("xpc: value %v overflows %s", src, dv.Type())
}

// into stores the next object in dv, as the package documentation
// describes. It reads the wire format directly, and skips the entries no
// struct field wants without converting them.
func (d *decoder) into(dv reflect.Value) error {
	start, startPort := d.off, d.next
	typ, err := d.u32()
	if err != nil {
		return err
	}
	// rewind lets value read the whole object.
	rewind := func() {
		d.off, d.next = start, startPort
	}
	switch typ {
	case wireNull:
		dv.SetZero()
		return nil
	case wireData:
		if dv.CanAddr() && dataFlagsOf(dv.Type())&ptrUnmarshalsData != 0 {
			return d.unmarshalData(dv)
		}
	}
	switch typ {
	case wireBool, wireInt64, wireUint64, wireDouble, wireDate, wireData,
		wireString, wireUUID, wireFD, wireArray, wireDictionary:
	default:
		rewind()
		_, err := d.value() // unsupported
		return err
	}

	// The special types, found by kind first: comparing types costs more.
	var want uint32
	switch dv.Kind() {
	case reflect.Struct:
		if dv.Type() == timeType {
			want = wireDate
		}
	case reflect.Array:
		if dv.Type() == uuidType {
			want = wireUUID
		}
	case reflect.Pointer:
		if dv.Type() == fileType {
			want = wireFD
		}
	}
	if want != 0 || dv.Kind() == reflect.Interface {
		if want != 0 && typ != want {
			return wireMismatch(typ, dv)
		}
		rewind()
		v, err := d.value()
		if err != nil {
			return err
		}
		sv := reflect.ValueOf(v)
		if !sv.Type().AssignableTo(dv.Type()) {
			return wireMismatch(typ, dv)
		}
		dv.Set(sv)
		return nil
	}

	switch dv.Kind() {
	case reflect.Pointer:
		if dv.IsNil() {
			dv.Set(reflect.New(dv.Type().Elem()))
		}
		rewind()
		return d.into(dv.Elem())
	case reflect.Struct:
		if typ != wireDictionary {
			break
		}
		n, err := d.container()
		if err != nil {
			return err
		}
		if err := d.intoStruct(dv, n); err != nil {
			return err
		}
		d.depth--
		return nil
	case reflect.Bool:
		if typ != wireBool {
			break
		}
		v, err := d.u32()
		dv.SetBool(v != 0)
		return err
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if typ != wireInt64 && typ != wireUint64 {
			break
		}
		u, err := d.u64()
		if err != nil {
			return err
		}
		if typ == wireUint64 && u > math.MaxInt64 {
			return overflow(u, dv)
		}
		if dv.OverflowInt(int64(u)) {
			return overflow(wireNumber(typ, u), dv)
		}
		dv.SetInt(int64(u))
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if typ != wireInt64 && typ != wireUint64 {
			break
		}
		u, err := d.u64()
		if err != nil {
			return err
		}
		if typ == wireInt64 && int64(u) < 0 {
			return overflow(int64(u), dv)
		}
		if dv.OverflowUint(u) {
			return overflow(wireNumber(typ, u), dv)
		}
		dv.SetUint(u)
		return nil
	case reflect.Float32, reflect.Float64:
		var f float64
		switch typ {
		case wireDouble:
			u, err := d.u64()
			if err != nil {
				return err
			}
			f = math.Float64frombits(u)
		case wireInt64:
			u, err := d.u64()
			if err != nil {
				return err
			}
			f = float64(int64(u))
		case wireUint64:
			u, err := d.u64()
			if err != nil {
				return err
			}
			f = float64(u)
		default:
			return wireMismatch(typ, dv)
		}
		dv.SetFloat(f)
		return nil
	case reflect.String:
		if typ != wireString {
			break
		}
		s, err := d.str()
		dv.SetString(s)
		return err
	case reflect.Slice:
		if typ == wireData && dv.Type().Elem().Kind() == reflect.Uint8 {
			b, err := d.dataValue() // a copy already
			if err != nil {
				return err
			}
			dv.SetBytes(b)
			return nil
		}
		if typ != wireArray {
			break
		}
		n, err := d.container()
		if err != nil {
			return err
		}
		// A new backing array, grown in place: reflect.MakeSlice would
		// also allocate a slice header. It grows as elements are read.
		dv.SetZero()
		dv.Grow(min(n, maxPrealloc))
		for i := range n {
			if i == dv.Cap() {
				dv.Grow(1)
			}
			dv.SetLen(i + 1)
			if err := d.into(dv.Index(i)); err != nil {
				return fmt.Errorf("xpc: index %d: %w", i, err)
			}
		}
		d.depth--
		return nil
	case reflect.Array:
		if typ == wireData && dv.Type().Elem().Kind() == reflect.Uint8 {
			b, err := d.dataValue()
			if err != nil {
				return err
			}
			if len(b) != dv.Len() {
				break
			}
			for i, c := range b {
				dv.Index(i).SetUint(uint64(c))
			}
			return nil
		}
		if typ != wireArray {
			break
		}
		n, err := d.container()
		if err != nil {
			return err
		}
		if n != dv.Len() {
			break
		}
		for i := range n {
			if err := d.into(dv.Index(i)); err != nil {
				return fmt.Errorf("xpc: index %d: %w", i, err)
			}
		}
		d.depth--
		return nil
	case reflect.Map:
		if typ != wireDictionary || dv.Type().Key().Kind() != reflect.String {
			break
		}
		n, err := d.container()
		if err != nil {
			return err
		}
		if dv.IsNil() {
			dv.Set(reflect.MakeMapWithSize(dv.Type(), min(n, maxPrealloc)))
		}
		// Reused for every entry: SetMapIndex copies them.
		kv := reflect.New(dv.Type().Key()).Elem()
		ev := reflect.New(dv.Type().Elem()).Elem()
		for range n {
			k, err := d.key()
			if err != nil {
				return err
			}
			ev.SetZero()
			if err := d.into(ev); err != nil {
				return fmt.Errorf("xpc: key %q: %w", k, err)
			}
			kv.SetString(k)
			dv.SetMapIndex(kv, ev)
		}
		d.depth--
		return nil
	}
	return wireMismatch(typ, dv)
}

// Field states in intoStruct.
const (
	unset = iota
	setFolded
	setExact
)

// intoStruct stores the next n dictionary entries in the fields of dv. As
// with decode, an entry named exactly like a field wins over entries that
// match it only case-insensitively.
func (d *decoder) intoStruct(dv reflect.Value, n int) error {
	fields := structFields(dv.Type())
	index := fieldIndex(dv.Type())
	var small [64]uint8
	state := small[:0]
	if len(fields) <= len(small) {
		state = small[:len(fields)]
	} else {
		state = make([]uint8, len(fields))
	}
	for range n {
		k, err := d.key()
		if err != nil {
			return err
		}
		start, startPort := d.off, d.next
		end, endPort := -1, 0
		set := func(i int) error {
			fv, err := fieldAlloc(dv, fields[i].index)
			if err != nil {
				return err
			}
			if state[i] == setFolded {
				fv.SetZero() // drop what a case-insensitive match stored
			}
			d.off, d.next = start, startPort
			if err := d.into(fv); err != nil {
				return fmt.Errorf("xpc: field %s: %w", fields[i].name, err)
			}
			end, endPort = d.off, d.next
			return nil
		}
		if exact := index[k]; len(exact) > 0 {
			for _, i := range exact {
				if err := set(i); err != nil {
					return err
				}
				state[i] = setExact
			}
		} else {
			for i, f := range fields {
				if state[i] != setExact && strings.EqualFold(f.name, k) {
					if err := set(i); err != nil {
						return err
					}
					state[i] = setFolded
				}
			}
		}
		if end < 0 {
			if err := d.skip(); err != nil {
				return err
			}
		} else {
			d.off, d.next = end, endPort
		}
	}
	return nil
}

// wireNumber returns the number u read for an object of type typ, for
// errors.
func wireNumber(typ uint32, u uint64) any {
	if typ == wireInt64 {
		return int64(u)
	}
	return u
}

func wireMismatch(typ uint32, dv reflect.Value) error {
	return fmt.Errorf("xpc: cannot decode %s into %s", wireType(typ), dv.Type())
}
