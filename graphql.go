package main

// A small GraphQL implementation: lexer, parser (operations, fragments,
// variables, input objects, enums, directives) and the executor in
// graphql_exec.go. Covers the query/mutation surface the forge exposes.

import (
	"fmt"
	"strconv"
	"strings"
)

// ---- lexer ----

const (
	tEOF = iota
	tName
	tString
	tInt
	tFloat
	tPunct
)

type gqlTok struct {
	kind int
	val  string
}

func gqlLex(src string) ([]gqlTok, error) {
	var toks []gqlTok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			toks = append(toks, gqlTok{tPunct, "..."})
			i += 3
		case strings.ContainsRune("{}()[]:!$=@|", rune(c)):
			toks = append(toks, gqlTok{tPunct, string(c)})
			i++
		case c == '"':
			if i+2 < len(src) && src[i+1] == '"' && src[i+2] == '"' {
				// block string
				end := strings.Index(src[i+3:], `"""`)
				if end < 0 {
					return nil, fmt.Errorf("unterminated block string")
				}
				toks = append(toks, gqlTok{tString, src[i+3 : i+3+end]})
				i += 3 + end + 3
				continue
			}
			var sb strings.Builder
			i++
			closed := false
			for i < len(src) {
				if src[i] == '"' {
					closed = true
					i++
					break
				}
				if src[i] == '\\' && i+1 < len(src) {
					i++
					switch src[i] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case 'b':
						sb.WriteByte('\b')
					case 'f':
						sb.WriteByte('\f')
					case '/':
						sb.WriteByte('/')
					case '\\':
						sb.WriteByte('\\')
					case '"':
						sb.WriteByte('"')
					case 'u':
						if i+4 < len(src) {
							if n, err := strconv.ParseUint(src[i+1:i+5], 16, 32); err == nil {
								sb.WriteRune(rune(n))
								i += 4
							}
						}
					}
					i++
					continue
				}
				sb.WriteByte(src[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, gqlTok{tString, sb.String()})
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			if src[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				if src[j] == '.' || src[j] == 'e' || src[j] == 'E' {
					isFloat = true
				}
				j++
			}
			kind := tInt
			if isFloat {
				kind = tFloat
			}
			toks = append(toks, gqlTok{kind, src[i:j]})
			i = j
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			j := i
			for j < len(src) && (src[j] == '_' || (src[j] >= '0' && src[j] <= '9') || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= 'A' && src[j] <= 'Z')) {
				j++
			}
			toks = append(toks, gqlTok{tName, src[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	toks = append(toks, gqlTok{tEOF, ""})
	return toks, nil
}

// ---- AST ----

const (
	vString = iota
	vInt
	vFloat
	vBool
	vNull
	vEnum
	vList
	vObject
	vVar
)

type gqlValue struct {
	kind int
	s    string
	list []*gqlValue
	obj  []gqlKV
}

type gqlKV struct {
	k string
	v *gqlValue
}

type gqlField struct {
	name     string
	alias    string
	args     []gqlKV
	sel      []*gqlField
	spread   string // fragment spread name ("" when not a spread)
	inlineOn string // inline fragment type condition ("" when not inline)
}

func (f *gqlField) key() string {
	if f.alias != "" {
		return f.alias
	}
	return f.name
}

func (f *gqlField) arg(name string) *gqlValue {
	for _, kv := range f.args {
		if kv.k == name {
			return kv.v
		}
	}
	return nil
}

type gqlOp struct {
	typ     string // "query" | "mutation"
	name    string
	varDefs map[string]*gqlValue // variable name -> default value (may be nil)
	sel     []*gqlField
}

type gqlFrag struct {
	on  string
	sel []*gqlField
}

type gqlDoc struct {
	ops   []*gqlOp
	frags map[string]*gqlFrag
}

// ---- parser ----

type gqlParser struct {
	toks []gqlTok
	pos  int
}

func (p *gqlParser) peek() gqlTok { return p.toks[p.pos] }
func (p *gqlParser) next() gqlTok { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) isPunct(s string) bool {
	t := p.peek()
	return t.kind == tPunct && t.val == s
}
func (p *gqlParser) expectPunct(s string) error {
	if !p.isPunct(s) {
		return fmt.Errorf("expected %q, got %q", s, p.peek().val)
	}
	p.pos++
	return nil
}

func gqlParse(src string) (*gqlDoc, error) {
	toks, err := gqlLex(src)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{toks: toks}
	doc := &gqlDoc{frags: map[string]*gqlFrag{}}
	for p.peek().kind != tEOF {
		t := p.peek()
		if t.kind == tPunct && t.val == "{" {
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.ops = append(doc.ops, &gqlOp{typ: "query", varDefs: map[string]*gqlValue{}, sel: sel})
			continue
		}
		if t.kind != tName {
			return nil, fmt.Errorf("unexpected token %q", t.val)
		}
		switch t.val {
		case "query", "mutation", "subscription":
			p.next()
			op := &gqlOp{typ: t.val, varDefs: map[string]*gqlValue{}}
			if p.peek().kind == tName {
				op.name = p.next().val
			}
			if p.isPunct("(") {
				if err := p.parseVarDefs(op); err != nil {
					return nil, err
				}
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			op.sel = sel
			doc.ops = append(doc.ops, op)
		case "fragment":
			p.next()
			nameTok := p.next()
			if nameTok.kind != tName {
				return nil, fmt.Errorf("fragment needs a name")
			}
			onTok := p.next()
			if onTok.kind != tName || onTok.val != "on" {
				return nil, fmt.Errorf("fragment needs 'on'")
			}
			typeTok := p.next()
			if typeTok.kind != tName {
				return nil, fmt.Errorf("fragment needs a type condition")
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.frags[nameTok.val] = &gqlFrag{on: typeTok.val, sel: sel}
		default:
			return nil, fmt.Errorf("unexpected definition %q", t.val)
		}
	}
	return doc, nil
}

func (p *gqlParser) parseVarDefs(op *gqlOp) error {
	if err := p.expectPunct("("); err != nil {
		return err
	}
	for !p.isPunct(")") {
		if err := p.expectPunct("$"); err != nil {
			return err
		}
		nameTok := p.next()
		if nameTok.kind != tName {
			return fmt.Errorf("variable needs a name")
		}
		if err := p.expectPunct(":"); err != nil {
			return err
		}
		if err := p.skipType(); err != nil {
			return err
		}
		var def *gqlValue
		if p.isPunct("=") {
			p.next()
			v, err := p.parseValue()
			if err != nil {
				return err
			}
			def = v
		}
		if err := p.skipDirectives(); err != nil {
			return err
		}
		op.varDefs[nameTok.val] = def
	}
	return p.expectPunct(")")
}

// skipType consumes a type reference: Name, [Type], with optional ! suffixes.
func (p *gqlParser) skipType() error {
	if p.isPunct("[") {
		p.next()
		if err := p.skipType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		t := p.next()
		if t.kind != tName {
			return fmt.Errorf("expected a type name")
		}
	}
	if p.isPunct("!") {
		p.next()
	}
	return nil
}

func (p *gqlParser) skipDirectives() error {
	for p.isPunct("@") {
		p.next()
		t := p.next()
		if t.kind != tName {
			return fmt.Errorf("directive needs a name")
		}
		if p.isPunct("(") {
			if _, err := p.parseArgs(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *gqlParser) parseArgs() ([]gqlKV, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var out []gqlKV
	for !p.isPunct(")") {
		nameTok := p.next()
		if nameTok.kind != tName {
			return nil, fmt.Errorf("argument needs a name")
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		out = append(out, gqlKV{nameTok.val, v})
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlField, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var out []*gqlField
	for !p.isPunct("}") {
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *gqlParser) parseField() (*gqlField, error) {
	if p.isPunct("...") {
		p.next()
		t := p.peek()
		if t.kind == tName && t.val == "on" {
			p.next()
			typeTok := p.next()
			if typeTok.kind != tName {
				return nil, fmt.Errorf("inline fragment needs a type")
			}
			f := &gqlField{inlineOn: typeTok.val}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			f.sel = sel
			return f, nil
		}
		nameTok := p.next()
		if nameTok.kind != tName {
			return nil, fmt.Errorf("fragment spread needs a name")
		}
		f := &gqlField{spread: nameTok.val}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		return f, nil
	}
	nameTok := p.next()
	if nameTok.kind != tName {
		return nil, fmt.Errorf("field needs a name, got %q", nameTok.val)
	}
	f := &gqlField{name: nameTok.val}
	if p.isPunct(":") {
		p.next()
		realTok := p.next()
		if realTok.kind != tName {
			return nil, fmt.Errorf("alias target needs a name")
		}
		f.alias = f.name
		f.name = realTok.val
	}
	if p.isPunct("(") {
		args, err := p.parseArgs()
		if err != nil {
			return nil, err
		}
		f.args = args
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	if p.isPunct("{") {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.sel = sel
	}
	return f, nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	t := p.next()
	switch t.kind {
	case tString:
		return &gqlValue{kind: vString, s: t.val}, nil
	case tInt:
		return &gqlValue{kind: vInt, s: t.val}, nil
	case tFloat:
		return &gqlValue{kind: vFloat, s: t.val}, nil
	case tName:
		switch t.val {
		case "true":
			return &gqlValue{kind: vBool, s: "true"}, nil
		case "false":
			return &gqlValue{kind: vBool, s: "false"}, nil
		case "null":
			return &gqlValue{kind: vNull}, nil
		default:
			return &gqlValue{kind: vEnum, s: t.val}, nil
		}
	case tPunct:
		switch t.val {
		case "$":
			nameTok := p.next()
			if nameTok.kind != tName {
				return nil, fmt.Errorf("variable needs a name")
			}
			return &gqlValue{kind: vVar, s: nameTok.val}, nil
		case "[":
			v := &gqlValue{kind: vList}
			for !p.isPunct("]") {
				item, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.list = append(v.list, item)
			}
			if err := p.expectPunct("]"); err != nil {
				return nil, err
			}
			return v, nil
		case "{":
			v := &gqlValue{kind: vObject}
			for !p.isPunct("}") {
				nameTok := p.next()
				if nameTok.kind != tName {
					return nil, fmt.Errorf("input object field needs a name")
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				item, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.obj = append(v.obj, gqlKV{nameTok.val, item})
			}
			if err := p.expectPunct("}"); err != nil {
				return nil, err
			}
			return v, nil
		}
	}
	return nil, fmt.Errorf("unexpected value token %q", t.val)
}
