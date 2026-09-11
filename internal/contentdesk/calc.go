package contentdesk

import (
	"fmt"
	"math"
	"strconv"
	"unicode"
)

// EvalFormula evaluates an arithmetic formula over named inputs: numbers,
// identifiers, + - * / ^, unary minus and parentheses. Anything else is an
// error — a formula the checker cannot recompute is a failed check, never a
// pass.
func EvalFormula(formula string, inputs map[string]float64) (float64, error) {
	p := &calcParser{src: []rune(formula), vars: inputs}
	v, err := p.expr()
	if err != nil {
		return 0, err
	}
	p.skip()
	if p.pos != len(p.src) {
		return 0, fmt.Errorf("unexpected %q at %d", string(p.src[p.pos]), p.pos)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("formula is not finite")
	}
	return v, nil
}

// CalcMatches recomputes c and compares to its stated result within
// max(0.01, 1e-6·|result|).
func CalcMatches(c Calc) (float64, bool, error) {
	in := make(map[string]float64, len(c.Inputs))
	for _, i := range c.Inputs {
		in[i.Name] = i.Value
	}
	got, err := EvalFormula(c.Formula, in)
	if err != nil {
		return 0, false, err
	}
	tol := math.Max(0.01, 1e-6*math.Abs(c.Result))
	return got, math.Abs(got-c.Result) <= tol, nil
}

type calcParser struct {
	src  []rune
	pos  int
	vars map[string]float64
}

func (p *calcParser) skip() {
	for p.pos < len(p.src) && unicode.IsSpace(p.src[p.pos]) {
		p.pos++
	}
}

func (p *calcParser) peek() rune {
	p.skip()
	if p.pos < len(p.src) {
		return p.src[p.pos]
	}
	return 0
}

func (p *calcParser) expr() (float64, error) {
	v, err := p.term()
	if err != nil {
		return 0, err
	}
	for {
		switch p.peek() {
		case '+':
			p.pos++
			r, err := p.term()
			if err != nil {
				return 0, err
			}
			v += r
		case '-':
			p.pos++
			r, err := p.term()
			if err != nil {
				return 0, err
			}
			v -= r
		default:
			return v, nil
		}
	}
}

func (p *calcParser) term() (float64, error) {
	v, err := p.power()
	if err != nil {
		return 0, err
	}
	for {
		switch p.peek() {
		case '*':
			p.pos++
			r, err := p.power()
			if err != nil {
				return 0, err
			}
			v *= r
		case '/':
			p.pos++
			r, err := p.power()
			if err != nil {
				return 0, err
			}
			if r == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			v /= r
		default:
			return v, nil
		}
	}
}

func (p *calcParser) power() (float64, error) {
	b, err := p.unary()
	if err != nil {
		return 0, err
	}
	if p.peek() == '^' {
		p.pos++
		e, err := p.power() // right-associative
		if err != nil {
			return 0, err
		}
		return math.Pow(b, e), nil
	}
	return b, nil
}

func (p *calcParser) unary() (float64, error) {
	if p.peek() == '-' {
		p.pos++
		v, err := p.unary()
		return -v, err
	}
	if p.peek() == '+' {
		p.pos++
		return p.unary()
	}
	return p.primary()
}

func (p *calcParser) primary() (float64, error) {
	c := p.peek()
	switch {
	case c == '(':
		p.pos++
		v, err := p.expr()
		if err != nil {
			return 0, err
		}
		if p.peek() != ')' {
			return 0, fmt.Errorf("missing )")
		}
		p.pos++
		return v, nil
	case unicode.IsDigit(c) || c == '.':
		start := p.pos
		for p.pos < len(p.src) && (unicode.IsDigit(p.src[p.pos]) || p.src[p.pos] == '.') {
			p.pos++
		}
		return strconv.ParseFloat(string(p.src[start:p.pos]), 64)
	case unicode.IsLetter(c) || c == '_':
		start := p.pos
		for p.pos < len(p.src) && (unicode.IsLetter(p.src[p.pos]) || unicode.IsDigit(p.src[p.pos]) || p.src[p.pos] == '_') {
			p.pos++
		}
		name := string(p.src[start:p.pos])
		v, ok := p.vars[name]
		if !ok {
			return 0, fmt.Errorf("unknown input %q", name)
		}
		return v, nil
	case c == 0:
		return 0, fmt.Errorf("unexpected end of formula")
	}
	return 0, fmt.Errorf("unexpected %q", string(c))
}
