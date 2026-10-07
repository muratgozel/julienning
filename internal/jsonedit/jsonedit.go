// Package jsonedit edits JSON objects and arrays by splicing bytes, never by
// decoding and re-encoding the document. Every member julienning does not
// touch keeps its exact bytes: key order, whitespace, number literals
// (1e3, 1.50), escapes and unicode all survive. Only the span of a value that
// is set, or the member that is inserted or deleted, changes.
//
// Nested edits work by opening the nested value as its own editor
// (Object.Object, Object.Array, Array.Object) and splicing its Bytes back with
// Set. New values are rendered in the style detected from their container:
// multi-line containers get indented values (json.Indent at the member's
// depth), single-line containers get compact values with the container's
// separators.
package jsonedit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Errors for type mismatches. Use errors.Is; the returned errors name the key
// or index involved.
var (
	ErrNotObject = errors.New("not a JSON object")
	ErrNotArray  = errors.New("not a JSON array")
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// defaultUnit is the indentation used when a document gives no hint.
const defaultUnit = "  "

// style is how members of one container are laid out.
type style struct {
	base      string // indentation of the line holding the opening bracket
	unit      string // one indentation level
	multiline bool   // members sit on their own lines
	colon     string // between a key and its value, e.g. ": "
	nl        string // line break, "\n" or "\r\n"
}

type item struct {
	key      string // decoded key; objects only
	start    int    // key start (objects) or value start (arrays)
	keyEnd   int    // just past the key's closing quote; objects only
	valStart int
	valEnd   int
}

// container is an object or array held in buf. For the top-level document,
// buf also holds the whitespace around the value.
type container struct {
	buf   []byte
	kind  byte // '{' or '['
	open  int
	close int
	items []item
	st    style
}

// Object edits a JSON object.
type Object struct{ c *container }

// Array edits a JSON array.
type Array struct{ c *container }

// Parse opens a JSON document whose top-level value is an object. A leading
// UTF-8 BOM is dropped (encoding/json rejects it). Surrounding whitespace is
// kept and returned by Bytes. Invalid JSON yields a syntax error; valid JSON
// of another type yields an error wrapping ErrNotObject.
func Parse(data []byte) (*Object, error) {
	data = bytes.TrimPrefix(data, utf8BOM)
	var probe json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	open := skipWS(data, 0)
	if open >= len(data) || data[open] != '{' {
		return nil, fmt.Errorf("top-level value is %w", ErrNotObject)
	}
	buf := append([]byte(nil), data...)
	st := style{
		base:      leadingIndent(buf, bytes.LastIndexByte(buf[:open], '\n')+1),
		unit:      defaultUnit,
		multiline: true, // an empty document grows pretty-printed
		colon:     ": ",
		nl:        detectNewline(buf),
	}
	c := &container{buf: buf, kind: '{', open: open}
	if err := c.scan(st); err != nil {
		return nil, err
	}
	return &Object{c: c}, nil
}

// Bytes returns the current document.
func (o *Object) Bytes() []byte { return append([]byte(nil), o.c.buf...) }

// Len is the number of members (duplicates counted).
func (o *Object) Len() int { return len(o.c.items) }

// Keys returns member keys in document order.
func (o *Object) Keys() []string {
	keys := make([]string, len(o.c.items))
	for i, it := range o.c.items {
		keys[i] = it.key
	}
	return keys
}

// index returns the last member with key (encoding/json semantics: the last
// duplicate wins), or -1.
func (o *Object) index(key string) int {
	for i := len(o.c.items) - 1; i >= 0; i-- {
		if o.c.items[i].key == key {
			return i
		}
	}
	return -1
}

// Has reports whether key is present.
func (o *Object) Has(key string) bool { return o.index(key) >= 0 }

// Get returns the raw bytes of key's value.
func (o *Object) Get(key string) ([]byte, bool) {
	i := o.index(key)
	if i < 0 {
		return nil, false
	}
	return o.c.value(i), true
}

// Set replaces key's value with raw, or appends a new member when key is
// absent. raw is inserted verbatim (it must be valid JSON), so a child
// editor's Bytes splice back byte-for-byte.
func (o *Object) Set(key string, raw []byte) error {
	if !json.Valid(raw) {
		return fmt.Errorf("set %q: value is not valid JSON", key)
	}
	if i := o.index(key); i >= 0 {
		return o.c.replace(i, raw)
	}
	kb, err := marshal(key)
	if err != nil {
		return err
	}
	return o.c.insert(append(append(kb, o.c.st.colon...), raw...))
}

// SetValue is Set with v rendered in the container's style at the member's
// depth. json.RawMessage values are re-indented, structs keep field order.
func (o *Object) SetValue(key string, v any) error {
	i := o.index(key)
	indent := o.c.memberIndent(i)
	raw, err := o.c.render(v, indent)
	if err != nil {
		return fmt.Errorf("set %q: %w", key, err)
	}
	return o.Set(key, raw)
}

// Delete removes every member with key; it reports whether one existed.
func (o *Object) Delete(key string) (bool, error) {
	deleted := false
	for i := o.index(key); i >= 0; i = o.index(key) {
		if err := o.c.remove(i); err != nil {
			return deleted, err
		}
		deleted = true
	}
	return deleted, nil
}

// Object opens key's value as an object editor. ok is false when key is
// absent; err wraps ErrNotObject when the value is another type.
func (o *Object) Object(key string) (child *Object, ok bool, err error) {
	i := o.index(key)
	if i < 0 {
		return nil, false, nil
	}
	c, err := o.c.child(i, '{')
	if err != nil {
		return nil, true, fmt.Errorf("%q is %w", key, ErrNotObject)
	}
	return &Object{c: c}, true, nil
}

// Array opens key's value as an array editor; see Object.
func (o *Object) Array(key string) (child *Array, ok bool, err error) {
	i := o.index(key)
	if i < 0 {
		return nil, false, nil
	}
	c, err := o.c.child(i, '[')
	if err != nil {
		return nil, true, fmt.Errorf("%q is %w", key, ErrNotArray)
	}
	return &Array{c: c}, true, nil
}

// Bytes returns the array's current bytes.
func (a *Array) Bytes() []byte { return append([]byte(nil), a.c.buf...) }

// Len is the number of elements.
func (a *Array) Len() int { return len(a.c.items) }

// Get returns the raw bytes of element i.
func (a *Array) Get(i int) []byte {
	if i < 0 || i >= len(a.c.items) {
		return nil
	}
	return a.c.value(i)
}

// Set replaces element i with raw (valid JSON, inserted verbatim).
func (a *Array) Set(i int, raw []byte) error {
	if i < 0 || i >= len(a.c.items) {
		return fmt.Errorf("set element %d: index out of range", i)
	}
	if !json.Valid(raw) {
		return fmt.Errorf("set element %d: value is not valid JSON", i)
	}
	return a.c.replace(i, raw)
}

// Append adds raw (valid JSON, inserted verbatim) as the last element.
func (a *Array) Append(raw []byte) error {
	if !json.Valid(raw) {
		return errors.New("append: value is not valid JSON")
	}
	return a.c.insert(raw)
}

// AppendValue appends v rendered in the array's style.
func (a *Array) AppendValue(v any) error {
	raw, err := a.c.render(v, a.c.memberIndent(-1))
	if err != nil {
		return fmt.Errorf("append: %w", err)
	}
	return a.Append(raw)
}

// Delete removes element i.
func (a *Array) Delete(i int) error {
	if i < 0 || i >= len(a.c.items) {
		return fmt.Errorf("delete element %d: index out of range", i)
	}
	return a.c.remove(i)
}

// Object opens element i as an object editor.
func (a *Array) Object(i int) (*Object, error) {
	if i < 0 || i >= len(a.c.items) {
		return nil, fmt.Errorf("element %d: index out of range", i)
	}
	c, err := a.c.child(i, '{')
	if err != nil {
		return nil, fmt.Errorf("element %d is %w", i, ErrNotObject)
	}
	return &Object{c: c}, nil
}

// Array opens element i as an array editor.
func (a *Array) Array(i int) (*Array, error) {
	if i < 0 || i >= len(a.c.items) {
		return nil, fmt.Errorf("element %d: index out of range", i)
	}
	c, err := a.c.child(i, '[')
	if err != nil {
		return nil, fmt.Errorf("element %d is %w", i, ErrNotArray)
	}
	return &Array{c: c}, nil
}

// --- container internals ---

func (c *container) value(i int) []byte {
	it := c.items[i]
	return append([]byte(nil), c.buf[it.valStart:it.valEnd]...)
}

// scan (re)builds items and close from buf[open:], and refreshes the style.
// inherited supplies what an empty container cannot tell us.
func (c *container) scan(inherited style) error {
	b := c.buf
	closeByte := byte('}')
	if c.kind == '[' {
		closeByte = ']'
	}
	c.items = c.items[:0]
	c.st = inherited
	i := skipWS(b, c.open+1)
	if i < len(b) && b[i] == closeByte {
		c.close = i
		c.st = c.detect(inherited)
		return nil
	}
	for {
		var it item
		if i >= len(b) {
			return errors.New("unexpected end of JSON")
		}
		if c.kind == '{' {
			if b[i] != '"' {
				return fmt.Errorf("offset %d: expected a key", i)
			}
			end, err := scanString(b, i)
			if err != nil {
				return err
			}
			var key string
			if err := json.Unmarshal(b[i:end], &key); err != nil {
				return fmt.Errorf("offset %d: bad key: %w", i, err)
			}
			it.key, it.start, it.keyEnd = key, i, end
			i = skipWS(b, end)
			if i >= len(b) || b[i] != ':' {
				return fmt.Errorf("offset %d: expected ':'", i)
			}
			i = skipWS(b, i+1)
		} else {
			it.start = i
		}
		end, err := scanValue(b, i)
		if err != nil {
			return err
		}
		it.valStart, it.valEnd = i, end
		c.items = append(c.items, it)
		i = skipWS(b, end)
		if i >= len(b) {
			return errors.New("unexpected end of JSON")
		}
		switch b[i] {
		case ',':
			i = skipWS(b, i+1)
		case closeByte:
			c.close = i
			c.st = c.detect(inherited)
			return nil
		default:
			return fmt.Errorf("offset %d: expected ',' or '%c'", i, closeByte)
		}
	}
}

// detect derives the layout from existing members, falling back to inherited.
func (c *container) detect(inherited style) style {
	st := inherited
	if len(c.items) == 0 {
		return st
	}
	first := c.items[0]
	st.multiline = bytes.ContainsRune(c.buf[c.open:first.start], '\n')
	if st.multiline {
		mi := c.lineIndent(first.start)
		if strings.HasPrefix(mi, st.base) && len(mi) > len(st.base) {
			st.unit = mi[len(st.base):]
		}
	}
	if c.kind == '{' {
		last := c.items[len(c.items)-1]
		st.colon = string(c.buf[last.keyEnd:last.valStart])
	}
	return st
}

// lineIndent is the leading whitespace of the line containing pos. A line
// that starts before this buffer (child editors) has the container's base.
func (c *container) lineIndent(pos int) string {
	nl := bytes.LastIndexByte(c.buf[:pos], '\n')
	if nl < 0 {
		return c.st.base
	}
	return leadingIndent(c.buf, nl+1)
}

// memberIndent is the indentation new or replaced values at member i are
// rendered with (i < 0: a member to be appended).
func (c *container) memberIndent(i int) string {
	switch {
	case i >= 0:
		return c.lineIndent(c.items[i].start)
	case len(c.items) > 0:
		return c.lineIndent(c.items[len(c.items)-1].start)
	default:
		return c.st.base + c.st.unit
	}
}

// child opens member i's value as a container of the given kind. The child
// inherits the layout so an empty nested value grows in the parent's style.
func (c *container) child(i int, kind byte) (*container, error) {
	v := c.value(i)
	if len(v) == 0 || v[0] != kind {
		return nil, errors.New("type mismatch")
	}
	st := c.st
	st.base = c.lineIndent(c.items[i].start)
	ch := &container{buf: v, kind: kind, open: 0}
	if err := ch.scan(st); err != nil {
		return nil, err
	}
	return ch, nil
}

// splice replaces buf[from:to] with repl and rescans.
func (c *container) splice(from, to int, repl []byte) error {
	nb := make([]byte, 0, len(c.buf)-(to-from)+len(repl))
	nb = append(nb, c.buf[:from]...)
	nb = append(nb, repl...)
	nb = append(nb, c.buf[to:]...)
	c.buf = nb
	return c.scan(c.st)
}

func (c *container) replace(i int, raw []byte) error {
	it := c.items[i]
	if bytes.Equal(c.buf[it.valStart:it.valEnd], raw) {
		return nil
	}
	return c.splice(it.valStart, it.valEnd, raw)
}

// insert appends member (key+colon+value for objects, value for arrays)
// after the last member, copying the separator already used between members.
func (c *container) insert(member []byte) error {
	n := len(c.items)
	if n == 0 {
		var repl []byte
		if c.st.multiline {
			repl = append(repl, c.st.nl...)
			repl = append(repl, c.st.base+c.st.unit...)
			repl = append(repl, member...)
			repl = append(repl, c.st.nl...)
			repl = append(repl, c.st.base...)
		} else {
			repl = member
		}
		return c.splice(c.open+1, c.close, repl)
	}
	var sep []byte
	if n >= 2 {
		sep = c.buf[c.items[n-2].valEnd:c.items[n-1].start]
	} else {
		sep = append([]byte{','}, c.buf[c.open+1:c.items[0].start]...)
	}
	repl := append(append([]byte(nil), sep...), member...)
	end := c.items[n-1].valEnd
	return c.splice(end, end, repl)
}

// remove deletes member i together with the comma that separated it.
func (c *container) remove(i int) error {
	n := len(c.items)
	switch {
	case n == 1:
		return c.splice(c.open+1, c.close, nil)
	case i < n-1:
		return c.splice(c.items[i].start, c.items[i+1].start, nil)
	default:
		return c.splice(c.items[i-1].valEnd, c.items[i].valEnd, nil)
	}
}

// render marshals v for a member whose line is indented with indent.
func (c *container) render(v any, indent string) ([]byte, error) {
	compact, err := marshal(v)
	if err != nil {
		return nil, err
	}
	if !c.st.multiline {
		return respace(compact, strings.TrimLeft(c.st.colon, " \t"), c.compactComma()), nil
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, indent, c.st.unit); err != nil {
		return nil, fmt.Errorf("indent value: %w", err)
	}
	b := out.Bytes()
	if c.st.nl != "\n" {
		b = bytes.ReplaceAll(b, []byte("\n"), []byte(c.st.nl))
	}
	return b, nil
}

// compactComma is the separator a single-line container uses between members.
func (c *container) compactComma() string {
	n := len(c.items)
	var sep string
	switch {
	case n >= 2:
		sep = string(c.buf[c.items[n-2].valEnd:c.items[n-1].start])
	case n == 1:
		sep = "," + string(c.buf[c.open+1:c.items[0].start])
	default:
		return ","
	}
	if strings.ContainsAny(sep, "\r\n") {
		return ","
	}
	return sep
}

// marshal is json.Marshal without HTML escaping (commands may contain &).
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("encode value: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// respace rewrites the separators of compact JSON (no insignificant
// whitespace) to the given colon and comma forms.
func respace(compact []byte, colon, comma string) []byte {
	if colon == ":" && comma == "," {
		return compact
	}
	out := make([]byte, 0, len(compact)+len(compact)/4)
	inString, escaped := false, false
	for _, ch := range compact {
		switch {
		case inString:
			out = append(out, ch)
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
		case ch == '"':
			inString = true
			out = append(out, ch)
		case ch == ':':
			out = append(out, colon...)
		case ch == ',':
			out = append(out, comma...)
		default:
			out = append(out, ch)
		}
	}
	return out
}

// --- scanning (input is already validated by encoding/json) ---

func isWS(ch byte) bool { return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' }

func skipWS(b []byte, i int) int {
	for i < len(b) && isWS(b[i]) {
		i++
	}
	return i
}

func leadingIndent(b []byte, lineStart int) string {
	j := lineStart
	for j < len(b) && (b[j] == ' ' || b[j] == '\t') {
		j++
	}
	return string(b[lineStart:j])
}

func detectNewline(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i > 0 && b[i-1] == '\r' {
		return "\r\n"
	}
	return "\n"
}

// scanString returns the index just past the string starting at b[i] == '"'.
func scanString(b []byte, i int) (int, error) {
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1, nil
		}
	}
	return 0, fmt.Errorf("offset %d: unterminated string", i)
}

// scanValue returns the index just past the value starting at b[i].
func scanValue(b []byte, i int) (int, error) {
	if i >= len(b) {
		return 0, errors.New("unexpected end of JSON")
	}
	switch b[i] {
	case '"':
		return scanString(b, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				end, err := scanString(b, j)
				if err != nil {
					return 0, err
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
		}
		return 0, fmt.Errorf("offset %d: unterminated container", i)
	default:
		j := i
		for j < len(b) && !isWS(b[j]) && b[j] != ',' && b[j] != '}' && b[j] != ']' {
			j++
		}
		if j == i {
			return 0, fmt.Errorf("offset %d: expected a value", i)
		}
		return j, nil
	}
}
