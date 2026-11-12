package report

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Dur renders a number of seconds compactly: 0.213s, 4.52s, 37.1s, 2m05s,
// 1h02m.
func Dur(s float64) string {
	switch {
	case math.IsNaN(s) || math.IsInf(s, 0):
		return "-"
	case s < 0:
		return "-" + Dur(-s)
	case s == 0:
		return "0s"
	case s < 0.9995:
		return fmt.Sprintf("%.3fs", s)
	case s < 9.995:
		return fmt.Sprintf("%.2fs", s)
	case s < 99.95:
		return fmt.Sprintf("%.1fs", s)
	case s < 3600:
		whole := int(math.Round(s))
		if whole >= 3600 {
			return "1h00m"
		}
		return fmt.Sprintf("%dm%02ds", whole/60, whole%60)
	}
	minutes := int(math.Round(s / 60))
	return fmt.Sprintf("%dh%02dm", minutes/60, minutes%60)
}

// Clock renders a simulation time as hh:mm:ss.
func Clock(s float64) string {
	whole := int(math.Floor(s))
	return fmt.Sprintf("%02d:%02d:%02d", whole/3600, whole/60%60, whole%60)
}

// Pct renders a share (0..1) as a percentage. Values that would round to
// 100% without being complete keep enough digits to show the gap.
func Pct(v float64) string {
	p := v * 100
	switch {
	case p >= 100:
		return "100%"
	case p > 99.9:
		return fmt.Sprintf("%.2f%%", math.Floor(p*100)/100)
	case p >= 10:
		return fmt.Sprintf("%.1f%%", p)
	}
	return fmt.Sprintf("%.2f%%", p)
}

// Num renders a count with thousands separators.
func Num(n int) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Money renders a cost.
func Money(v float64) string {
	if v >= 1000 {
		return "$" + Num(int(math.Round(v)))
	}
	return fmt.Sprintf("$%.2f", v)
}

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

// Sparkline renders values as a line of block characters scaled to
// [0, max]. Zero is drawn as a space-saving low block so that the line keeps
// its length.
func Sparkline(vals []float64) (line string, top float64) {
	for _, v := range vals {
		top = math.Max(top, v)
	}
	var b strings.Builder
	for _, v := range vals {
		if top <= 0 || v <= 0 {
			b.WriteRune(sparkRunes[0])
			continue
		}
		i := int(math.Ceil(v/top*float64(len(sparkRunes)-1) - 1e-9))
		i = min(max(i, 0), len(sparkRunes)-1)
		b.WriteRune(sparkRunes[i])
	}
	return b.String(), top
}

// bucketise reduces a (time, value) series to n buckets over [0, duration]
// with the given reducer ("max" or "mean").
func bucketise(ts, vs []float64, duration float64, n int, mode string) []float64 {
	out := make([]float64, n)
	if duration <= 0 || n == 0 {
		return out
	}
	counts := make([]int, n)
	for i, t := range ts {
		b := int(t / duration * float64(n))
		b = min(max(b, 0), n-1)
		if mode == "max" {
			out[b] = math.Max(out[b], vs[i])
		} else {
			out[b] += vs[i]
		}
		counts[b]++
	}
	last := 0.0
	for b := range out {
		if counts[b] == 0 {
			out[b] = last // carry the previous level through empty buckets
			continue
		}
		if mode != "max" {
			out[b] /= float64(counts[b])
		}
		last = out[b]
	}
	return out
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// table renders rows as aligned text columns. right lists the columns that
// are right-aligned.
func table(rows [][]string, right map[int]bool, indent string) string {
	widths := map[int]int{}
	for _, row := range rows {
		for i, cell := range row {
			if w := len([]rune(cell)); w > widths[i] {
				widths[i] = w
			}
		}
	}
	var b strings.Builder
	for _, row := range rows {
		var line strings.Builder
		line.WriteString(indent)
		for i, cell := range row {
			pad := strings.Repeat(" ", widths[i]-len([]rune(cell)))
			last := i == len(row)-1
			switch {
			case right[i]:
				line.WriteString(pad + cell)
			case last:
				line.WriteString(cell)
			default:
				line.WriteString(cell + pad)
			}
			if !last {
				line.WriteString("  ")
			}
		}
		b.WriteString(strings.TrimRight(line.String(), " ") + "\n")
	}
	return b.String()
}

// mdTable renders rows as a GitHub-flavoured Markdown table; the first row
// is the header.
func mdTable(rows [][]string, right map[int]bool) string {
	var b strings.Builder
	for r, row := range rows {
		b.WriteString("| " + strings.Join(row, " | ") + " |\n")
		if r == 0 {
			seps := make([]string, len(row))
			for i := range row {
				if right[i] {
					seps[i] = "---:"
				} else {
					seps[i] = "---"
				}
			}
			b.WriteString("| " + strings.Join(seps, " | ") + " |\n")
		}
	}
	return b.String()
}
