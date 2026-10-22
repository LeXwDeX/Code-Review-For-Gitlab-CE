package dag

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// Conditions use a deliberately small grammar:
//
//	expr    := and ("||" and)*
//	and     := compare ("&&" compare)*
//	compare := <node>.output.<path> ("=="|"!=") <json literal>
//
// && binds tighter than ||. There is no evaluation library and no grouping.
// A missing output path makes the comparison false for both operators: an
// absent optional output never satisfies a condition.

type condition interface {
	eval(wf WorkflowView) bool
}

type conditionOr struct{ terms []condition }

func (c conditionOr) eval(wf WorkflowView) bool {
	for _, term := range c.terms {
		if term.eval(wf) {
			return true
		}
	}
	return false
}

type conditionAnd struct{ terms []condition }

func (c conditionAnd) eval(wf WorkflowView) bool {
	for _, term := range c.terms {
		if !term.eval(wf) {
			return false
		}
	}
	return true
}

type conditionCompare struct {
	node    string
	path    string
	not     bool
	literal any
}

func (c conditionCompare) eval(wf WorkflowView) bool {
	view, ok := wf.Nodes[c.node]
	if !ok || len(view.Output) == 0 && view.Status != NodeCompleted {
		return false
	}
	left, present := outputPath(view.Output, c.path)
	if !present {
		return false
	}
	equal := literalEqual(left, c.literal)
	if c.not {
		return !equal
	}
	return equal
}

func parseCondition(expr string, ids, reachable map[string]bool) (condition, error) {
	p := &conditionParser{expr: expr, ids: ids, reachable: reachable}
	cond, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.expr) {
		return nil, fmt.Errorf("unexpected token at byte %d in %q", p.pos, expr)
	}
	return cond, nil
}

type conditionParser struct {
	expr      string
	pos       int
	ids       map[string]bool
	reachable map[string]bool
}

func (p *conditionParser) skipSpace() {
	for p.pos < len(p.expr) {
		switch p.expr[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *conditionParser) parseOr() (condition, error) {
	first, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	terms := []condition{first}
	for {
		p.skipSpace()
		if !strings.HasPrefix(p.expr[p.pos:], "||") {
			break
		}
		p.pos += 2
		next, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		terms = append(terms, next)
	}
	if len(terms) == 1 {
		return terms[0], nil
	}
	return conditionOr{terms: terms}, nil
}

func (p *conditionParser) parseAnd() (condition, error) {
	first, err := p.parseCompare()
	if err != nil {
		return nil, err
	}
	terms := []condition{first}
	for {
		p.skipSpace()
		if !strings.HasPrefix(p.expr[p.pos:], "&&") {
			break
		}
		p.pos += 2
		next, err := p.parseCompare()
		if err != nil {
			return nil, err
		}
		terms = append(terms, next)
	}
	if len(terms) == 1 {
		return terms[0], nil
	}
	return conditionAnd{terms: terms}, nil
}

func (p *conditionParser) parseCompare() (condition, error) {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.expr) {
		switch p.expr[p.pos] {
		case ' ', '\t', '\n', '\r', '=', '!', '&', '|':
			goto lhsDone
		default:
			p.pos++
		}
	}
lhsDone:
	lhs := p.expr[start:p.pos]
	if lhs == "" {
		return nil, fmt.Errorf("expected output reference at byte %d", start)
	}
	node, path, err := splitOutputRef(lhs, p.ids)
	if err != nil {
		return nil, err
	}
	if !p.reachable[node] {
		return nil, fmt.Errorf("node %q is not a reachable dependency", node)
	}
	if !pathPattern.MatchString(path) {
		return nil, fmt.Errorf("invalid output path %q", path)
	}

	p.skipSpace()
	var not bool
	switch {
	case strings.HasPrefix(p.expr[p.pos:], "=="):
		p.pos += 2
	case strings.HasPrefix(p.expr[p.pos:], "!="):
		not = true
		p.pos += 2
	default:
		return nil, fmt.Errorf("expected == or != after %q", lhs)
	}

	p.skipSpace()
	if p.pos >= len(p.expr) {
		return nil, fmt.Errorf("expected JSON literal after operator")
	}
	dec := json.NewDecoder(strings.NewReader(p.expr[p.pos:]))
	dec.UseNumber()
	var literal any
	if err := dec.Decode(&literal); err != nil {
		return nil, fmt.Errorf("invalid JSON literal: %v", err)
	}
	p.pos += int(dec.InputOffset())
	return conditionCompare{node: node, path: path, not: not, literal: literal}, nil
}

func evalCondition(expr string, wf WorkflowView) bool {
	ids := make(map[string]bool, len(wf.Nodes))
	reachable := make(map[string]bool, len(wf.Nodes))
	for id := range wf.Nodes {
		ids[id] = true
		reachable[id] = true
	}
	cond, err := parseCondition(expr, ids, reachable)
	if err != nil {
		return false
	}
	return cond.eval(wf)
}

func outputPath(raw json.RawMessage, path string) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var current any
	if err := dec.Decode(&current); err != nil {
		return nil, false
	}
	for _, segment := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func literalEqual(left, right any) bool {
	if lnum, ok := left.(json.Number); ok {
		rnum, ok := right.(json.Number)
		if !ok {
			return false
		}
		leftInt, lok := new(big.Rat).SetString(lnum.String())
		rightInt, rok := new(big.Rat).SetString(rnum.String())
		return lok && rok && leftInt.Cmp(rightInt) == 0
	}
	switch l := left.(type) {
	case nil:
		return right == nil
	case bool:
		r, ok := right.(bool)
		return ok && l == r
	case string:
		r, ok := right.(string)
		return ok && l == r
	case []any:
		r, ok := right.([]any)
		if !ok || len(l) != len(r) {
			return false
		}
		for i := range l {
			if !literalEqual(l[i], r[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		r, ok := right.(map[string]any)
		if !ok || len(l) != len(r) {
			return false
		}
		for key, value := range l {
			other, exists := r[key]
			if !exists || !literalEqual(value, other) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
