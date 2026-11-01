// Package model holds the Plex Media Server response models and the encoder
// that renders one model as either PMS-style XML or PMS-style JSON.
//
// Models are plain structs annotated with `plex` struct tags:
//
//	plex:"name"            attribute, always emitted
//	plex:"name,omitempty"  attribute, omitted when it has the zero value
//	plex:"Name"            on a []struct / []*struct field: child elements; in
//	                       JSON an array under "Name"
//	plex:"Name,object"     on a struct / *struct field: a single child; in JSON
//	                       an object (not an array) under "Name"
//	plex:",inline"         on an embedded or named struct field: its fields are
//	                       flattened into the parent element
//	plex:",attrs"          on an []Attr field: extra attributes, in order
//	plex:"-"               ignored
//
// Pointer scalars (*int, *bool, ...) are omitted when nil and emitted (even
// when zero) otherwise. In XML booleans render as "1"/"0" like PMS; in JSON
// they are true/false. A struct may implement ElementNamer to choose its XML
// element name (e.g. Metadata renders as <Video> or <Directory> depending on
// its type); otherwise the field tag name is used.
package model

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Format selects the wire encoding.
type Format int

const (
	XML Format = iota
	JSON
)

// ContentType returns the Content-Type PMS uses for the format.
func (f Format) ContentType() string {
	if f == JSON {
		return "application/json"
	}
	return "text/xml;charset=utf-8"
}

// ElementNamer lets a model pick its XML element name dynamically.
type ElementNamer interface {
	XMLElementName() string
}

// Attr is an extra attribute for fields tagged `plex:",attrs"`. Value may be
// a string, bool, any integer or float type.
type Attr struct {
	Name  string
	Value any
}

// Marshal renders root (a struct or pointer to struct) in the given format.
// The root element name is taken from ElementNamer, defaulting to
// "MediaContainer".
func Marshal(root any, f Format) ([]byte, error) {
	var buf bytes.Buffer
	if err := encode(&buf, root, f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Encode writes root to w in the given format. It encodes the full document
// before writing to w so an encoding error never leaves partial output on w.
func Encode(w io.Writer, root any, f Format) error {
	b, err := Marshal(root, f)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func encode(w io.Writer, root any, f Format) error {
	v := reflect.ValueOf(root)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return fmt.Errorf("model: nil root")
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("model: root must be a struct, got %s", v.Kind())
	}
	name := elementName(v, "MediaContainer")
	bw := bufio.NewWriter(w)
	var err error
	if f == JSON {
		bw.WriteString(`{`)
		writeJSONString(bw, name)
		bw.WriteString(`:`)
		err = encodeJSONObject(bw, v)
		bw.WriteString("}\n")
	} else {
		bw.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
		err = encodeXMLElement(bw, v, name, true)
	}
	if err != nil {
		return err
	}
	return bw.Flush()
}

// ---------------------------------------------------------------------------
// Type metadata cache

type fieldKind int

const (
	fkAttr fieldKind = iota
	fkChildren
	fkObject
	fkInline
	fkAttrs
)

type fieldInfo struct {
	index     []int
	name      string
	kind      fieldKind
	omitEmpty bool
}

var typeCache sync.Map // reflect.Type -> []fieldInfo

var attrSliceType = reflect.TypeFor[[]Attr]()

func fieldsOf(t reflect.Type) []fieldInfo {
	if c, ok := typeCache.Load(t); ok {
		return c.([]fieldInfo)
	}
	var out []fieldInfo
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		tag, hasTag := sf.Tag.Lookup("plex")
		if tag == "-" {
			continue
		}
		if !sf.IsExported() && !sf.Anonymous {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		fi := fieldInfo{index: sf.Index, name: name}
		for _, o := range strings.Split(opts, ",") {
			switch o {
			case "omitempty":
				fi.omitEmpty = true
			case "object":
				fi.kind = fkObject
			case "inline":
				fi.kind = fkInline
			case "attrs":
				fi.kind = fkAttrs
			}
		}
		ft := sf.Type
		if fi.kind == fkAttr {
			switch {
			case ft == attrSliceType:
				fi.kind = fkAttrs
			case sf.Anonymous && derefType(ft).Kind() == reflect.Struct && name == "":
				fi.kind = fkInline
			case ft.Kind() == reflect.Slice && derefType(ft.Elem()).Kind() == reflect.Struct:
				fi.kind = fkChildren
			case derefType(ft).Kind() == reflect.Struct:
				fi.kind = fkObject
			}
		}
		if fi.name == "" && fi.kind != fkInline && fi.kind != fkAttrs {
			if !hasTag && !sf.IsExported() {
				continue
			}
			fi.name = lowerFirst(sf.Name)
			if fi.kind == fkChildren || fi.kind == fkObject {
				fi.name = sf.Name
			}
		}
		out = append(out, fi)
	}
	typeCache.Store(t, out)
	return out
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func elementName(v reflect.Value, def string) string {
	if v.CanInterface() {
		if n, ok := v.Interface().(ElementNamer); ok {
			if s := n.XMLElementName(); s != "" {
				return s
			}
		}
	}
	if v.CanAddr() {
		if n, ok := v.Addr().Interface().(ElementNamer); ok {
			if s := n.XMLElementName(); s != "" {
				return s
			}
		}
	}
	return def
}

// scalar formats an attribute value. ok=false means "omit".
func scalar(v reflect.Value, omitEmpty bool, f Format) (s string, quoted bool, ok bool) {
	if v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return "", false, false
		}
		v = v.Elem()
		omitEmpty = false
		if v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
			return scalar(v, false, f)
		}
	}
	if omitEmpty && v.IsZero() {
		return "", false, false
	}
	switch v.Kind() {
	case reflect.String:
		return v.String(), true, true
	case reflect.Bool:
		if f == JSON {
			return strconv.FormatBool(v.Bool()), false, true
		}
		if v.Bool() {
			return "1", false, true
		}
		return "0", false, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10), false, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(v.Uint(), 10), false, true
	case reflect.Float32, reflect.Float64:
		x := v.Float()
		if math.IsNaN(x) || math.IsInf(x, 0) {
			x = 0
		}
		return strconv.FormatFloat(x, 'f', -1, 64), false, true
	}
	return "", false, false
}

func anyScalar(x any, f Format) (string, bool, bool) {
	if x == nil {
		return "", false, false
	}
	return scalar(reflect.ValueOf(x), false, f)
}

// ---------------------------------------------------------------------------
// XML

type xmlParts struct {
	attrs    []Attr // pre-formatted (Value is string)
	children []xmlChild
}

type xmlChild struct {
	v    reflect.Value
	name string
}

func collectXML(v reflect.Value, p *xmlParts) {
	for _, fi := range fieldsOf(v.Type()) {
		fv := v.FieldByIndex(fi.index)
		switch fi.kind {
		case fkAttr:
			if s, _, ok := scalar(fv, fi.omitEmpty, XML); ok {
				p.attrs = append(p.attrs, Attr{fi.name, s})
			}
		case fkAttrs:
			for _, a := range fv.Interface().([]Attr) {
				if s, _, ok := anyScalar(a.Value, XML); ok {
					p.attrs = append(p.attrs, Attr{a.Name, s})
				}
			}
		case fkInline:
			for fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					break
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				collectXML(fv, p)
			}
		case fkObject:
			for fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					break
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				p.children = append(p.children, xmlChild{fv, elementName(fv, fi.name)})
			}
		case fkChildren:
			for i := 0; i < fv.Len(); i++ {
				ev := fv.Index(i)
				for ev.Kind() == reflect.Pointer {
					if ev.IsNil() {
						break
					}
					ev = ev.Elem()
				}
				if ev.Kind() != reflect.Struct {
					continue
				}
				p.children = append(p.children, xmlChild{ev, elementName(ev, fi.name)})
			}
		}
	}
}

func encodeXMLElement(w *bufio.Writer, v reflect.Value, name string, root bool) error {
	var p xmlParts
	collectXML(v, &p)
	p.attrs = sortAttrs(v, p.attrs, func(a Attr) string { return a.Name })
	w.WriteByte('<')
	w.WriteString(name)
	seen := make(map[string]bool, len(p.attrs))
	for _, a := range p.attrs {
		// A repeated attribute makes the document ill-formed (strict
		// parsers reject it); keep the first, as the JSON encoder does.
		if seen[a.Name] {
			continue
		}
		seen[a.Name] = true
		w.WriteByte(' ')
		w.WriteString(a.Name)
		w.WriteString(`="`)
		writeXMLAttr(w, a.Value.(string))
		w.WriteByte('"')
	}
	// PMS self-closes empty children but always writes the root as an
	// open/close pair.
	if len(p.children) == 0 && !root {
		w.WriteString(" />\n")
		return nil
	}
	w.WriteString(">\n")
	for _, c := range p.children {
		if err := encodeXMLElement(w, c.v, c.name, false); err != nil {
			return err
		}
	}
	w.WriteString("</")
	w.WriteString(name)
	w.WriteString(">\n")
	return nil
}

func writeXMLAttr(w *bufio.Writer, s string) {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '&':
			w.WriteString("&amp;")
		case r == '<':
			w.WriteString("&lt;")
		case r == '>':
			w.WriteString("&gt;")
		case r == '"':
			w.WriteString("&#34;")
		case r == '\'':
			w.WriteString("&#39;")
		case r == '\n':
			w.WriteString("&#10;")
		case r == '\r':
			w.WriteString("&#13;")
		case r == '\t':
			w.WriteString("&#9;")
		case r == utf8.RuneError && size == 1:
			// invalid UTF-8: drop
		case r < 0x20, r == 0xFFFE, r == 0xFFFF:
			// not allowed in XML 1.0: drop
		default:
			w.WriteString(s[i : i+size])
		}
		i += size
	}
}

// ---------------------------------------------------------------------------
// JSON

type jsonGroup struct {
	name   string
	object bool
	items  []reflect.Value
}

type jsonParts struct {
	attrs  []jsonAttr
	groups []*jsonGroup
}

type jsonAttr struct {
	name   string
	value  string
	quoted bool
}

func (p *jsonParts) group(name string, object bool) *jsonGroup {
	for _, g := range p.groups {
		if g.name == name {
			return g
		}
	}
	g := &jsonGroup{name: name, object: object}
	p.groups = append(p.groups, g)
	return g
}

func collectJSON(v reflect.Value, p *jsonParts) {
	for _, fi := range fieldsOf(v.Type()) {
		fv := v.FieldByIndex(fi.index)
		switch fi.kind {
		case fkAttr:
			if s, q, ok := scalar(fv, fi.omitEmpty, JSON); ok {
				p.attrs = append(p.attrs, jsonAttr{fi.name, s, q})
			}
		case fkAttrs:
			for _, a := range fv.Interface().([]Attr) {
				if s, q, ok := anyScalar(a.Value, JSON); ok {
					p.attrs = append(p.attrs, jsonAttr{a.Name, s, q})
				}
			}
		case fkInline:
			for fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					break
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				collectJSON(fv, p)
			}
		case fkObject:
			for fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					break
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				g := p.group(fi.name, true)
				g.items = append(g.items, fv)
			}
		case fkChildren:
			if fv.Len() == 0 {
				continue
			}
			g := p.group(fi.name, false)
			for i := 0; i < fv.Len(); i++ {
				ev := fv.Index(i)
				for ev.Kind() == reflect.Pointer {
					if ev.IsNil() {
						break
					}
					ev = ev.Elem()
				}
				if ev.Kind() == reflect.Struct {
					g.items = append(g.items, ev)
				}
			}
		}
	}
}

func encodeJSONObject(w *bufio.Writer, v reflect.Value) error {
	var p jsonParts
	collectJSON(v, &p)
	p.attrs = sortAttrs(v, p.attrs, func(a jsonAttr) string { return a.name })
	p.attrs = quoteAttrs(v, p.attrs)
	w.WriteByte('{')
	first := true
	sep := func() {
		if !first {
			w.WriteByte(',')
		}
		first = false
	}
	seen := make(map[string]bool, len(p.attrs))
	for _, a := range p.attrs {
		if seen[a.name] {
			continue // first occurrence wins, as in XML parsers that keep one
		}
		seen[a.name] = true
		sep()
		writeJSONString(w, a.name)
		w.WriteByte(':')
		if a.quoted {
			writeJSONString(w, a.value)
		} else {
			w.WriteString(a.value)
		}
	}
	for _, g := range p.groups {
		if len(g.items) == 0 {
			continue
		}
		sep()
		writeJSONString(w, g.name)
		w.WriteByte(':')
		if g.object && len(g.items) == 1 {
			if err := encodeJSONObject(w, g.items[0]); err != nil {
				return err
			}
			continue
		}
		w.WriteByte('[')
		for i, it := range g.items {
			if i > 0 {
				w.WriteByte(',')
			}
			if err := encodeJSONObject(w, it); err != nil {
				return err
			}
		}
		w.WriteByte(']')
	}
	w.WriteByte('}')
	return nil
}

func writeJSONString(w *bufio.Writer, s string) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // strings never fail
	b := buf.Bytes()
	w.Write(b[:len(b)-1]) // trim newline
}
