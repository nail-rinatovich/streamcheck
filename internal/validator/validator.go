// Package validator validates bounded records without retaining the dataset.
package validator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

const DefaultMaxRecord = 1 << 20

var ErrRecordTooLarge = errors.New("record exceeds max_record_bytes")

type Options struct {
	MaxRecordBytes int
	SampleLimit    int
}
type ColumnStats struct {
	Missing int64 `json:"missing"`
	Type    int64 `json:"type"`
	Range   int64 `json:"range"`
	Pattern int64 `json:"pattern"`
	Extra   int64 `json:"extra"`
}
type Sample struct {
	Row    int64  `json:"row"`
	Column string `json:"column"`
	Rule   string `json:"rule"`
}
type Report struct {
	Format      string                  `json:"format"`
	Complete    bool                    `json:"complete"`
	Rows        int64                   `json:"rows"`
	ValidRows   int64                   `json:"valid_rows"`
	InvalidRows int64                   `json:"invalid_rows"`
	Errors      int64                   `json:"errors"`
	ParseErrors int64                   `json:"parse_errors"`
	Bytes       int64                   `json:"bytes_read"`
	DurationMS  int64                   `json:"duration_ms"`
	Columns     map[string]*ColumnStats `json:"columns"`
	Samples     []Sample                `json:"samples"`
}

type counter struct {
	io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) { n, e := c.Reader.Read(p); c.n += int64(n); return n, e }

// records bounds allocation BEFORE appending input. CSV supports quoted multiline
// fields; a malformed/oversized CSV record aborts because recovery is ambiguous.
type records struct {
	r   *bufio.Reader
	max int
	csv bool
}

func (r *records) next(ctx context.Context) ([]byte, error) {
	var out []byte
	quoted := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		part, err := r.r.ReadSlice('\n')
		if len(out)+len(part) > r.max {
			return nil, ErrRecordTooLarge
		}
		out = append(out, part...)
		if r.csv {
			for _, b := range part {
				if b == '"' {
					quoted = !quoted
				}
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF && len(out) > 0 {
				return out, nil
			}
			return nil, err
		}
		if !r.csv || !quoted {
			return out, nil
		}
	}
}

func Validate(ctx context.Context, input io.Reader, format string, schema Schema, opts Options) (report Report, retErr error) {
	start := time.Now()
	c := &counter{Reader: input}
	report = Report{Format: format, Columns: map[string]*ColumnStats{}, Samples: []Sample{}}
	defer func() { report.Bytes = c.n; report.DurationMS = time.Since(start).Milliseconds() }()
	if format != "csv" && format != "jsonl" {
		return report, errors.New("format must be csv or jsonl")
	}
	// Recompile and validate even schemas constructed by Go callers.
	data, err := json.Marshal(schema)
	if err != nil {
		return report, err
	}
	schema, err = ParseSchema(data)
	if err != nil {
		return report, err
	}
	if opts.MaxRecordBytes == 0 {
		opts.MaxRecordBytes = DefaultMaxRecord
	}
	if opts.MaxRecordBytes < 1 || opts.MaxRecordBytes > 16<<20 || opts.SampleLimit < 0 || opts.SampleLimit > 100 {
		return report, errors.New("invalid record or sample limit")
	}
	for _, col := range schema.Columns {
		report.Columns[col.Name] = &ColumnStats{}
	}
	report.Columns["$extra"] = &ColumnStats{}
	record := records{r: bufio.NewReaderSize(c, 32<<10), max: opts.MaxRecordBytes, csv: format == "csv"}
	var header []string
	if format == "csv" {
		b, e := record.next(ctx)
		if e != nil {
			return report, fmt.Errorf("CSV header: %w", e)
		}
		b = bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf})
		header, e = parseCSV(b)
		if e != nil {
			return report, fmt.Errorf("CSV header: %w", e)
		}
		seen := map[string]bool{}
		for _, h := range header {
			if h == "" || seen[h] {
				return report, errors.New("CSV header contains an empty or duplicate column")
			}
			seen[h] = true
		}
		for _, col := range schema.Columns {
			if col.Required && !seen[col.Name] {
				return report, fmt.Errorf("CSV header missing required column %q", col.Name)
			}
		}
	}
	for {
		b, e := record.next(ctx)
		if e == io.EOF {
			report.Complete = true
			return report, nil
		}
		if e != nil {
			return report, e
		}
		report.Rows++
		before := report.Errors
		values := map[string]any{}
		if format == "csv" {
			cells, e := parseCSV(b)
			if e != nil {
				report.parseError(opts)
				return report, fmt.Errorf("CSV row %d: %w", report.Rows, e)
			}
			if len(cells) != len(header) {
				report.parseError(opts)
				continue
			}
			for i, h := range header {
				values[h] = cells[i]
			}
		} else {
			// Decoder preserves integer precision and rejects trailing JSON/duplicate keys.
			var e error
			values, e = parseObject(b)
			if e != nil {
				report.parseError(opts)
				continue
			}
		}
		known := map[string]bool{}
		for _, col := range schema.Columns {
			known[col.Name] = true
			v, ok := values[col.Name]
			if !ok || v == nil || (format == "csv" && v == "") {
				if col.Required {
					report.Columns[col.Name].Missing++
					report.add(col.Name, "missing", opts)
				}
				continue
			}
			n, s, ok := checkType(v, col.Type, format)
			if !ok {
				report.Columns[col.Name].Type++
				report.add(col.Name, "type", opts)
				continue
			}
			if (col.Min != nil && n < *col.Min) || (col.Max != nil && n > *col.Max) {
				report.Columns[col.Name].Range++
				report.add(col.Name, "range", opts)
			}
			if col.pattern != nil && !col.pattern.MatchString(s) {
				report.Columns[col.Name].Pattern++
				report.add(col.Name, "pattern", opts)
			}
		}
		if !schema.AllowExtra {
			for name := range values {
				if !known[name] {
					report.Columns["$extra"].Extra++
					report.add("$extra", "extra", opts)
				}
			}
		}
		if report.Errors > before {
			report.InvalidRows++
		} else {
			report.ValidRows++
		}
	}
}
func parseCSV(b []byte) ([]string, error) {
	r := csv.NewReader(bytes.NewReader(b))
	r.FieldsPerRecord = -1
	return r.Read()
}
func parseObject(b []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	t, e := d.Token()
	if e != nil || t != json.Delim('{') {
		return nil, errors.New("expected JSON object")
	}
	m := map[string]any{}
	for d.More() {
		t, e = d.Token()
		if e != nil {
			return nil, e
		}
		key, ok := t.(string)
		if !ok {
			return nil, errors.New("invalid key")
		}
		if _, ok = m[key]; ok {
			return nil, errors.New("duplicate JSON key")
		}
		var v any
		if e = d.Decode(&v); e != nil {
			return nil, e
		}
		m[key] = v
	}
	if _, e = d.Token(); e != nil {
		return nil, e
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	return m, nil
}
func (r *Report) add(col, rule string, o Options) {
	r.Errors++
	if len(r.Samples) < o.SampleLimit {
		r.Samples = append(r.Samples, Sample{r.Rows, col, rule})
	}
}
func (r *Report) parseError(o Options) {
	r.ParseErrors++
	r.InvalidRows++
	r.add("$record", "parse", o)
}
func checkType(v any, typ, format string) (float64, string, bool) {
	s, isString := v.(string)
	if typ == "string" {
		return 0, s, isString
	}
	if typ == "date" {
		if !isString {
			return 0, "", false
		}
		_, e := time.Parse("2006-01-02", s)
		return 0, s, e == nil
	}
	if typ == "boolean" {
		if format == "csv" {
			return 0, s, s == "true" || s == "false"
		}
		_, ok := v.(bool)
		return 0, "", ok
	}
	if format == "jsonl" {
		num, ok := v.(json.Number)
		if !ok {
			return 0, "", false
		}
		s = string(num)
	}
	if typ == "integer" {
		i, e := strconv.ParseInt(s, 10, 64)
		return float64(i), s, e == nil
	}
	n, e := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return n, s, e == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
}
