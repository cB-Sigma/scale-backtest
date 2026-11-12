// Package promql parses the small subset of PromQL that scale-backtest can
// evaluate against simulated engine metrics.
//
// Supported shapes (label matchers are parsed and reported, not evaluated):
//
//	metric                      a single gauge series
//	sum(metric)                 also avg, max, min
//	rate(counter[2m])           per-second rate of a single counter series
//	sum(rate(counter[2m]))      also avg, max, min
//	<any of the above> / 4      and * with a numeric literal on the right
//
// Everything else is refused with an error that says what is supported.
package promql

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Query is a parsed query.
type Query struct {
	// Raw is the query as written.
	Raw string
	// Agg is "", "sum", "avg", "max" or "min".
	Agg string
	// Rate is true when the selector is wrapped in rate(...[window]).
	Rate bool
	// WindowSeconds is the rate window.
	WindowSeconds float64
	// Metric is the metric name of the selector.
	Metric string
	// Matchers is the raw text between { and }, if any.
	Matchers string
	// Scale is a constant factor applied to the result (1 when absent).
	Scale float64
}

// UnsupportedError is returned for queries outside the supported subset.
type UnsupportedError struct {
	Query  string
	Reason string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("unsupported PromQL %q: %s (supported: metric, sum|avg|max|min(metric), rate(counter[w]), sum|avg|max|min(rate(counter[w])), optionally followed by * or / a number)", e.Query, e.Reason)
}

type parser struct {
	s   string
	pos int
}

func (p *parser) skipSpace() {
	for p.pos < len(p.s) && unicode.IsSpace(rune(p.s[p.pos])) {
		p.pos++
	}
}

func (p *parser) peek() byte {
	p.skipSpace()
	if p.pos >= len(p.s) {
		return 0
	}
	return p.s[p.pos]
}

func (p *parser) eat(c byte) bool {
	if p.peek() == c {
		p.pos++
		return true
	}
	return false
}

func isIdentStart(c byte) bool {
	return c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func (p *parser) ident() string {
	p.skipSpace()
	start := p.pos
	if p.pos < len(p.s) && isIdentStart(p.s[p.pos]) {
		p.pos++
		for p.pos < len(p.s) && isIdentChar(p.s[p.pos]) {
			p.pos++
		}
	}
	return p.s[start:p.pos]
}

var aggregations = map[string]bool{"sum": true, "avg": true, "max": true, "min": true}

// Parse parses a query or returns an *UnsupportedError.
func Parse(query string) (*Query, error) {
	q := &Query{Raw: query, Scale: 1}
	p := &parser{s: strings.TrimSpace(query)}
	fail := func(format string, args ...any) (*Query, error) {
		return nil, &UnsupportedError{Query: query, Reason: fmt.Sprintf(format, args...)}
	}
	if p.s == "" {
		return fail("empty query")
	}

	name := p.ident()
	if name == "" {
		return fail("expected a metric name or function at %q", p.rest())
	}
	closeAgg := false
	if aggregations[name] && p.peekAfterIdentIsCall() {
		q.Agg = name
		p.eat('(')
		closeAgg = true
		name = p.ident()
		if name == "" {
			return fail("expected a metric name or rate() inside %s()", q.Agg)
		}
	} else if aggregations[name] {
		if next := p.peekWord(); next == "by" || next == "without" {
			return fail("grouping with %s is not supported; the query must return a single value", next)
		}
	}

	switch {
	case name == "rate" && p.peek() == '(':
		p.eat('(')
		q.Rate = true
		metric := p.ident()
		if metric == "" {
			return fail("expected a counter name inside rate()")
		}
		q.Metric = metric
		if err := p.matchers(q); err != nil {
			return fail("%s", err)
		}
		if !p.eat('[') {
			return fail("rate() needs a range such as [2m]")
		}
		end := strings.IndexByte(p.s[p.pos:], ']')
		if end < 0 {
			return fail("unterminated range")
		}
		w, err := ParseDuration(strings.TrimSpace(p.s[p.pos : p.pos+end]))
		if err != nil || w <= 0 {
			return fail("bad range %q", p.s[p.pos:p.pos+end])
		}
		q.WindowSeconds = w
		p.pos += end + 1
		if !p.eat(')') {
			return fail("expected ) after the rate range")
		}
	case p.peek() == '(':
		return fail("function %s() is not supported", name)
	default:
		q.Metric = name
		if err := p.matchers(q); err != nil {
			return fail("%s", err)
		}
		if p.peek() == '[' {
			return fail("a range selector is only supported inside rate()")
		}
	}

	if closeAgg {
		if !p.eat(')') {
			return fail("expected ) to close %s(", q.Agg)
		}
		if next := p.peekWord(); next == "by" || next == "without" {
			return fail("grouping with %s is not supported; the query must return a single value", next)
		}
	}

	if c := p.peek(); c == '*' || c == '/' {
		p.pos++
		p.skipSpace()
		num := strings.TrimSpace(p.s[p.pos:])
		v, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return fail("only a numeric literal may follow %c, got %q", c, num)
		}
		if c == '/' {
			if v == 0 {
				return fail("division by zero")
			}
			q.Scale = 1 / v
		} else {
			q.Scale = v
		}
		p.pos = len(p.s)
	}
	if p.peek() != 0 {
		return fail("unexpected %q", p.rest())
	}
	return q, nil
}

func (p *parser) rest() string {
	p.skipSpace()
	return p.s[p.pos:]
}

func (p *parser) peekAfterIdentIsCall() bool {
	rest := strings.TrimLeftFunc(p.s[p.pos:], unicode.IsSpace)
	return strings.HasPrefix(rest, "(")
}

func (p *parser) peekWord() string {
	save := p.pos
	w := p.ident()
	p.pos = save
	return w
}

func (p *parser) matchers(q *Query) error {
	if !p.eat('{') {
		return nil
	}
	depth := 0
	inQuote := byte(0)
	start := p.pos
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		switch {
		case inQuote != 0:
			switch c {
			case '\\':
				p.pos++
			case inQuote:
				inQuote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			inQuote = c
		case c == '{':
			depth++
		case c == '}':
			if depth == 0 {
				q.Matchers = strings.TrimSpace(p.s[start:p.pos])
				p.pos++
				return nil
			}
			depth--
		}
		p.pos++
	}
	return fmt.Errorf("unterminated label matchers")
}

// ParseDuration parses a Prometheus-style duration such as 30s, 2m, 1h30m or
// 1d. Go-style durations with fractions are accepted too.
func ParseDuration(s string) (float64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d.Seconds(), nil
	}
	units := map[string]float64{"ms": 0.001, "s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800, "y": 31536000}
	total := 0.0
	i := 0
	for i < len(s) {
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		n, _ := strconv.ParseFloat(s[i:j], 64)
		k := j
		for k < len(s) && (s[k] < '0' || s[k] > '9') {
			k++
		}
		u, ok := units[s[j:k]]
		if !ok {
			return 0, fmt.Errorf("bad duration unit in %q", s)
		}
		total += n * u
		i = k
	}
	return total, nil
}

// String renders the query in canonical form.
func (q *Query) String() string {
	sel := q.Metric
	if q.Matchers != "" {
		sel += "{" + q.Matchers + "}"
	}
	if q.Rate {
		sel = fmt.Sprintf("rate(%s[%gs])", sel, q.WindowSeconds)
	}
	if q.Agg != "" {
		sel = q.Agg + "(" + sel + ")"
	}
	if q.Scale != 1 {
		sel += fmt.Sprintf(" * %g", q.Scale)
	}
	return sel
}
