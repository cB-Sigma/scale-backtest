// Package trace reads, writes, summarises and transforms request traces.
package trace

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Request is one inference request.
type Request struct {
	// Arrival is the arrival time in seconds from the start of the trace.
	Arrival      float64
	PromptTokens int
	OutputTokens int
	Model        string
	Tenant       string
}

// Trace is an ordered list of requests.
type Trace struct {
	// Name is a short label (usually the file name).
	Name string
	// Format is the format the trace was read from.
	Format string
	// Start is the wall-clock time of Arrival 0, when the source had
	// absolute timestamps.
	Start time.Time
	// Requests are sorted by arrival.
	Requests []Request
	// Duration is the length of the trace in seconds. It is at least the
	// last arrival.
	Duration float64
	// Approximate is true when arrivals were synthesised from aggregate
	// series rather than recorded per request.
	Approximate bool
	// Notes carries provenance, such as how arrivals were synthesised.
	Notes []string
}

// Formats accepted by Load.
const (
	FormatAuto  = "auto"
	FormatCSV   = "csv"
	FormatJSONL = "jsonl"
	FormatAzure = "azure"
)

// DurationMarker is the comment that records the length of a trace when it is
// longer than its last arrival (a trace may end with a quiet period).
const DurationMarker = "# duration_seconds:"

// ApproximateMarker is the comment that marks a CSV trace as synthesised from
// aggregate data. Load restores Trace.Approximate from it.
const ApproximateMarker = "# approximate:"

// Load reads a trace file. format is one of the Format constants.
func Load(path, format string) (*Trace, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("trace: %w", err)
	}
	defer func() { _ = f.Close() }()
	t, err := Read(f, format, path)
	if err != nil {
		return nil, fmt.Errorf("trace %s: %w", path, err)
	}
	t.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return t, nil
}

// Read reads a trace from r. nameHint is used to pick a format when format
// is "auto".
func Read(r io.Reader, format, nameHint string) (*Trace, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if format == "" || format == FormatAuto {
		format = sniff(data, nameHint)
	}
	var t *Trace
	switch format {
	case FormatCSV:
		t, err = readCSV(data, false)
	case FormatAzure:
		t, err = readCSV(data, true)
	case FormatJSONL:
		t, err = readJSONL(data)
	default:
		return nil, fmt.Errorf("unknown trace format %q (csv, jsonl, azure)", format)
	}
	if err != nil {
		return nil, err
	}
	t.Format = format
	t.normalise()
	if len(t.Requests) == 0 {
		return nil, errors.New("the trace contains no requests")
	}
	return t, nil
}

func sniff(data []byte, name string) string {
	first := firstDataLine(data)
	lower := strings.ToLower(first)
	switch {
	case strings.HasPrefix(strings.TrimSpace(first), "{"):
		return FormatJSONL
	case strings.Contains(lower, "contexttokens") && strings.Contains(lower, "generatedtokens"):
		return FormatAzure
	case strings.HasSuffix(strings.ToLower(name), ".jsonl") || strings.HasSuffix(strings.ToLower(name), ".ndjson"):
		return FormatJSONL
	}
	return FormatCSV
}

func firstDataLine(data []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

// column aliases, lower-cased with '_', '-' and spaces removed.
var (
	tsCols     = []string{"timestamp", "arrival", "arrivaltime", "arrivals", "time", "ts", "t", "requesttime"}
	promptCols = []string{"prompttokens", "inputtokens", "contexttokens", "prompt", "numprompttokens", "requesttokens"}
	outputCols = []string{"outputtokens", "generatedtokens", "completiontokens", "output", "numoutputtokens", "responsetokens", "numdecodetokens"}
	modelCols  = []string{"model", "modelname", "modelid"}
	tenantCols = []string{"tenant", "tenantid", "user", "userid", "customer"}
)

func normKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "\xef\xbb\xbf")
	return strings.NewReplacer("_", "", "-", "", " ", "").Replace(s)
}

func findCol(header []string, names []string) int {
	for i, h := range header {
		k := normKey(h)
		for _, n := range names {
			if k == n {
				return i
			}
		}
	}
	return -1
}

type rawRequest struct {
	abs    time.Time
	rel    float64
	isAbs  bool
	prompt int
	output int
	model  string
	tenant string
}

func readCSV(data []byte, azure bool) (*Trace, error) {
	t := &Trace{}
	// Strip comment lines, remembering the approximation marker.
	var clean bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			if strings.HasPrefix(trimmed, ApproximateMarker) {
				t.Approximate = true
				t.Notes = append(t.Notes, strings.TrimSpace(strings.TrimPrefix(trimmed, ApproximateMarker)))
			}
			if rest, ok := strings.CutPrefix(trimmed, DurationMarker); ok {
				if d, err := strconv.ParseFloat(strings.TrimSpace(rest), 64); err == nil && d > 0 {
					t.Duration = d
				}
			}
			continue
		}
		clean.WriteString(line)
		clean.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	rd := csv.NewReader(&clean)
	rd.FieldsPerRecord = -1
	rd.TrimLeadingSpace = true
	header, err := rd.Read()
	if err != nil {
		return nil, fmt.Errorf("reading the CSV header: %w", err)
	}
	ts, pc, oc := findCol(header, tsCols), findCol(header, promptCols), findCol(header, outputCols)
	mc, tc := findCol(header, modelCols), findCol(header, tenantCols)
	if azure {
		// The Azure LLM inference traces have exactly these columns.
		ts, pc, oc = findCol(header, []string{"timestamp"}), findCol(header, []string{"contexttokens"}), findCol(header, []string{"generatedtokens"})
		mc, tc = -1, -1
	}
	if ts < 0 || pc < 0 || oc < 0 {
		if azure {
			return nil, fmt.Errorf("an Azure LLM inference trace needs the columns TIMESTAMP, ContextTokens, GeneratedTokens; found %v", header)
		}
		return nil, fmt.Errorf("the CSV header needs a timestamp column, a prompt-token column and an output-token column (for example timestamp,prompt_tokens,output_tokens); found %v", header)
	}
	var raws []rawRequest
	line := 1
	for {
		rec, err := rd.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		line++
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", line, err)
		}
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue
		}
		need := max(ts, pc, oc)
		if len(rec) <= need {
			return nil, fmt.Errorf("row %d: expected at least %d columns, got %d", line, need+1, len(rec))
		}
		var r rawRequest
		if err := parseTimestamp(rec[ts], &r); err != nil {
			return nil, fmt.Errorf("row %d: %w", line, err)
		}
		if r.prompt, err = parseTokens(rec[pc]); err != nil {
			return nil, fmt.Errorf("row %d: prompt tokens: %w", line, err)
		}
		if r.output, err = parseTokens(rec[oc]); err != nil {
			return nil, fmt.Errorf("row %d: output tokens: %w", line, err)
		}
		if mc >= 0 && mc < len(rec) {
			r.model = strings.TrimSpace(rec[mc])
		}
		if tc >= 0 && tc < len(rec) {
			r.tenant = strings.TrimSpace(rec[tc])
		}
		raws = append(raws, r)
	}
	if err := t.fromRaw(raws); err != nil {
		return nil, err
	}
	return t, nil
}

type jsonRequest struct {
	Timestamp json.RawMessage `json:"timestamp"`
	Arrival   json.RawMessage `json:"arrival"`
	Time      json.RawMessage `json:"time"`

	PromptTokens     *float64 `json:"prompt_tokens"`
	InputTokens      *float64 `json:"input_tokens"`
	OutputTokens     *float64 `json:"output_tokens"`
	CompletionTokens *float64 `json:"completion_tokens"`

	Model  string `json:"model"`
	Tenant string `json:"tenant"`
	User   string `json:"user"`
}

func readJSONL(data []byte) (*Trace, error) {
	t := &Trace{}
	var raws []rawRequest
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var jr jsonRequest
		if err := json.Unmarshal([]byte(text), &jr); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		tsRaw := jr.Timestamp
		if tsRaw == nil {
			tsRaw = jr.Arrival
		}
		if tsRaw == nil {
			tsRaw = jr.Time
		}
		if tsRaw == nil {
			return nil, fmt.Errorf("line %d: no timestamp (expected \"timestamp\" or \"arrival\")", line)
		}
		tsText := strings.Trim(string(tsRaw), `"`)
		var r rawRequest
		if err := parseTimestamp(tsText, &r); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		prompt, output := jr.PromptTokens, jr.OutputTokens
		if prompt == nil {
			prompt = jr.InputTokens
		}
		if output == nil {
			output = jr.CompletionTokens
		}
		if prompt == nil || output == nil {
			return nil, fmt.Errorf("line %d: need prompt_tokens and output_tokens", line)
		}
		if *prompt < 0 || *output < 0 {
			return nil, fmt.Errorf("line %d: token counts must not be negative", line)
		}
		r.prompt, r.output = int(*prompt), int(*output)
		r.model = jr.Model
		r.tenant = jr.Tenant
		if r.tenant == "" {
			r.tenant = jr.User
		}
		raws = append(raws, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := t.fromRaw(raws); err != nil {
		return nil, err
	}
	return t, nil
}

func parseTokens(s string) (int, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("negative token count %d", n)
		}
		return n, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("bad token count %q", s)
	}
	return int(f), nil
}

// Absolute timestamp layouts. The first two are the layouts of the Azure LLM
// inference traces of 2023 ("2023-11-16 18:15:46.6805900") and 2024
// ("2024-05-10 00:00:00.009930+00:00").
var tsLayouts = []string{
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999 Z0700",
	"2006-01-02 15:04:05.999999999 MST",
}

func parseTimestamp(s string, r *rawRequest) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("empty timestamp")
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		if f < 0 {
			return fmt.Errorf("negative timestamp %q", s)
		}
		r.rel = f
		return nil
	}
	for _, layout := range tsLayouts {
		if ts, err := time.Parse(layout, s); err == nil {
			r.abs, r.isAbs = ts, true
			return nil
		}
	}
	return fmt.Errorf("cannot parse timestamp %q (use RFC 3339, \"2006-01-02 15:04:05.000\", or seconds as a number)", s)
}

func (t *Trace) fromRaw(raws []rawRequest) error {
	if len(raws) == 0 {
		return nil
	}
	abs := raws[0].isAbs
	var origin time.Time
	for i, r := range raws {
		if r.isAbs != abs {
			return fmt.Errorf("request %d: the trace mixes absolute and numeric timestamps", i+1)
		}
		if abs && (origin.IsZero() || r.abs.Before(origin)) {
			origin = r.abs
		}
	}
	// Numeric timestamps are seconds; a trace in Unix time is re-based on
	// its first request, a trace that already starts near zero is kept.
	minRel := raws[0].rel
	for _, r := range raws {
		if r.rel < minRel {
			minRel = r.rel
		}
	}
	base := 0.0
	if !abs && minRel > 86400*365 {
		base = minRel
		sec := int64(minRel)
		origin = time.Unix(sec, int64((minRel-float64(sec))*1e9)).UTC()
	}
	t.Start = origin
	t.Requests = make([]Request, len(raws))
	for i, r := range raws {
		arr := r.rel - base
		if abs {
			arr = r.abs.Sub(origin).Seconds()
		}
		t.Requests[i] = Request{Arrival: arr, PromptTokens: r.prompt, OutputTokens: r.output, Model: r.model, Tenant: r.tenant}
	}
	return nil
}

// normalise sorts requests and makes Duration cover the last arrival.
func (t *Trace) normalise() {
	sort.SliceStable(t.Requests, func(i, j int) bool { return t.Requests[i].Arrival < t.Requests[j].Arrival })
	if n := len(t.Requests); n > 0 && t.Requests[n-1].Arrival > t.Duration {
		t.Duration = t.Requests[n-1].Arrival
	}
}

// Normalise restores the invariants after requests were modified.
func (t *Trace) Normalise() { t.normalise() }

// WriteCSV writes the trace in the native CSV format: seconds from the trace
// start, token counts and, when present, model and tenant.
func (t *Trace) WriteCSV(w io.Writer) error {
	bw := bufio.NewWriter(w)
	if t.Approximate {
		note := "arrivals synthesised from aggregate series"
		if len(t.Notes) > 0 {
			note = t.Notes[0]
		}
		fmt.Fprintf(bw, "%s %s\n", ApproximateMarker, note)
	}
	if n := len(t.Requests); n == 0 || t.Duration > t.Requests[n-1].Arrival {
		fmt.Fprintf(bw, "%s %s\n", DurationMarker, strconv.FormatFloat(t.Duration, 'f', -1, 64))
	}
	extra := false
	for _, r := range t.Requests {
		if r.Model != "" || r.Tenant != "" {
			extra = true
			break
		}
	}
	cw := csv.NewWriter(bw)
	header := []string{"timestamp", "prompt_tokens", "output_tokens"}
	if extra {
		header = append(header, "model", "tenant")
	}
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, r := range t.Requests {
		rec := []string{strconv.FormatFloat(r.Arrival, 'f', 3, 64), strconv.Itoa(r.PromptTokens), strconv.Itoa(r.OutputTokens)}
		if extra {
			rec = append(rec, r.Model, r.Tenant)
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}
	return bw.Flush()
}

// Clone returns a deep copy.
func (t *Trace) Clone() *Trace {
	c := *t
	c.Requests = append([]Request(nil), t.Requests...)
	c.Notes = append([]string(nil), t.Notes...)
	return &c
}
