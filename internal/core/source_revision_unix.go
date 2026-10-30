//go:build !windows

package core

import (
	"os"
	"reflect"
)

func statNumber(v reflect.Value, name string) (uint64, bool) {
	f := v.FieldByName(name)
	if !f.IsValid() {
		return 0, false
	}
	switch f.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return f.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return uint64(f.Int()), true
	default:
		return 0, false
	}
}

func platformFileIdentity(_ *os.File, info os.FileInfo) (uint64, uint64, bool) {
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return 0, 0, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return 0, 0, false
	}
	dev, okDev := statNumber(v, "Dev")
	ino, okIno := statNumber(v, "Ino")
	return dev, ino, okDev && okIno
}
