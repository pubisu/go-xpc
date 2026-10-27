package xpc

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"
)

// The types the codec maps to XPC types of their own rather than by kind.
var (
	timeType = reflect.TypeFor[time.Time]()
	uuidType = reflect.TypeFor[UUID]()
	fileType = reflect.TypeFor[*os.File]()
	endType  = reflect.TypeFor[Endpoint]()
)

// A field is an exported struct field as a dictionary entry.
type field struct {
	name      string
	index     []int
	omitEmpty bool
	data      dataFlags // the data interfaces the field's type implements
}

var fieldCache sync.Map // reflect.Type -> []field

// structFields lists the exported fields of t, including fields promoted
// from embedded structs, with their dictionary keys.
func structFields(t reflect.Type) []field {
	if fs, ok := fieldCache.Load(t); ok {
		return fs.([]field)
	}
	var fs []field
	for _, sf := range reflect.VisibleFields(t) {
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("xpc")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if sf.Anonymous && name == "" {
			ft := sf.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				continue // its fields are promoted
			}
		}
		if name == "" {
			name = sf.Name
		}
		fs = append(fs, field{name: name, index: sf.Index, omitEmpty: opts == "omitempty", data: dataFlagsOf(sf.Type)})
	}
	actual, _ := fieldCache.LoadOrStore(t, fs)
	return actual.([]field)
}

// fieldAlloc is like FieldByIndex but allocates nil embedded pointers.
func fieldAlloc(v reflect.Value, index []int) (reflect.Value, error) {
	for i, x := range index {
		if i > 0 && v.Kind() == reflect.Pointer {
			if v.IsNil() {
				if !v.CanSet() {
					return reflect.Value{}, errors.New("xpc: cannot set embedded pointer to unexported struct")
				}
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = v.Field(x)
	}
	return v, nil
}

var fieldIndexCache sync.Map // reflect.Type -> map[string][]int

// fieldIndex maps the dictionary keys of t's fields to their indexes in
// structFields(t).
func fieldIndex(t reflect.Type) map[string][]int {
	if m, ok := fieldIndexCache.Load(t); ok {
		return m.(map[string][]int)
	}
	m := make(map[string][]int)
	for i, f := range structFields(t) {
		m[f.name] = append(m[f.name], i)
	}
	actual, _ := fieldIndexCache.LoadOrStore(t, m)
	return actual.(map[string][]int)
}
