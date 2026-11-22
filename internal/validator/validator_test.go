package validator

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func schema(t *testing.T, raw string) Schema {
	t.Helper()
	s, e := ParseSchema([]byte(raw))
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestValidationCounts(t *testing.T) {
	s := schema(t, `{"columns":[{"name":"id","type":"integer","required":true,"min":1},{"name":"email","type":"string","pattern":"@"},{"name":"age","type":"integer","max":120},{"name":"active","type":"boolean"}]}`)
	fixtures := []struct{ format, input string }{
		{"csv", "id,email,age,active\n1,a@b,29,true\n0,broken,999,true\n,ok@b,20,YES\n1,ok@b,20\n"},
		{"jsonl", "{\"id\":1,\"email\":\"a@b\",\"age\":29,\"active\":true}\n{\"id\":0,\"email\":\"broken\",\"age\":999,\"active\":true}\n{\"id\":null,\"email\":\"ok@b\",\"age\":20,\"active\":\"YES\"}\nbroken\n"},
	}
	for _, f := range fixtures {
		t.Run(f.format, func(t *testing.T) {
			r, e := Validate(context.Background(), strings.NewReader(f.input), f.format, s, Options{SampleLimit: 2})
			if e != nil {
				t.Fatal(e)
			}
			if !r.Complete || r.Rows != 4 || r.InvalidRows != 3 || r.ValidRows != 1 || r.Errors != 6 || r.ParseErrors != 1 || len(r.Samples) != 2 {
				t.Fatalf("unexpected report: %+v", r)
			}
			if r.Columns["id"].Range != 1 || r.Columns["id"].Missing != 1 || r.Columns["active"].Type != 1 {
				t.Fatal(r.Columns)
			}
		})
	}
}
func TestCSVMultilineAndEscaping(t *testing.T) {
	s := schema(t, `{"columns":[{"name":"note","type":"string","required":true}]}`)
	in := "\ufeffnote\r\n\"line 1\nline 2 with \"\"quotes\"\"\"\r\nlast"
	r, e := Validate(context.Background(), strings.NewReader(in), "csv", s, Options{})
	if e != nil || r.Rows != 2 || r.Errors != 0 {
		t.Fatalf("%+v %v", r, e)
	}
}
func TestJSONLStrictTypesAndRecovery(t *testing.T) {
	s := schema(t, `{"columns":[{"name":"id","type":"integer","required":true}]}`)
	in := `{"id":9223372036854775807}` + "\n" + `{"id":"1"}` + "\n" + `{"id":1,"id":2}` + "\n" + `{"id":1} {"id":2}` + "\n[]\nnull\n\n" + `{"id":2,"x":1}` + "\n" + `{"id":3}`
	r, e := Validate(context.Background(), strings.NewReader(in), "jsonl", s, Options{})
	if e != nil || r.Rows != 9 || r.ValidRows != 2 || r.ParseErrors != 5 || r.Columns["id"].Type != 1 || r.Columns["$extra"].Extra != 1 {
		t.Fatalf("%+v %v", r, e)
	}
}
func TestBoundedRecord(t *testing.T) {
	for _, format := range []string{"csv", "jsonl"} {
		t.Run(format, func(t *testing.T) {
			in := strings.Repeat("x", 1025)
			if format == "csv" {
				in = "id\n" + in
			}
			r, e := Validate(context.Background(), strings.NewReader(in), format, schema(t, `{"columns":[{"name":"id","type":"string"}]}`), Options{MaxRecordBytes: 1024})
			if !errors.Is(e, ErrRecordTooLarge) || r.Complete {
				t.Fatalf("%+v %v", r, e)
			}
		})
	}
}
func TestCancelAndReaderError(t *testing.T) {
	s := schema(t, `{"columns":[{"name":"id","type":"integer"}]}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e := Validate(ctx, strings.NewReader("{}"), "jsonl", s, Options{})
	if !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	_, e = Validate(context.Background(), failedReader{}, "jsonl", s, Options{})
	if !errors.Is(e, io.ErrUnexpectedEOF) {
		t.Fatal(e)
	}
}

type failedReader struct{}

func (failedReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestHeaderAndSchemaRejections(t *testing.T) {
	for _, raw := range []string{`{}`, `{"columns":[{"name":"x","type":"wat"}]}`, `{"columns":[{"name":"x","type":"string","min":0}]}`, `{"columns":[{"name":"x","type":"string","pattern":"["}]}`, `{"columns":[{"name":"x","type":"string"},{"name":"x","type":"integer"}]}`, `{"columns":[{"name":"$extra","type":"string"}]}`, `{"columns":[{"name":"x","type":"string"}],"typo":true}`, `{"columns":[{"name":"x","type":"string"}]} {}`} {
		if _, e := ParseSchema([]byte(raw)); e == nil {
			t.Fatal("accepted", raw)
		}
	}
	s := schema(t, `{"columns":[{"name":"id","type":"integer","required":true}]}`)
	for _, in := range []string{"id,id\n1,2\n", "other\n1\n", "id\n\"unterminated\n"} {
		r, e := Validate(context.Background(), strings.NewReader(in), "csv", s, Options{})
		if e == nil || r.Complete {
			t.Fatal("accepted", in)
		}
	}
}
func TestRecordBoundaryAndRules(t *testing.T) {
	s := schema(t, `{"columns":[{"name":"n","type":"number"},{"name":"d","type":"date"},{"name":"s","type":"string","required":true}]}`)
	r, e := Validate(context.Background(), strings.NewReader("n,d,s\nNaN,2025-02-29,\nInf,2024-02-29,hello\n1.5,2024-02-29,ok\n"), "csv", s, Options{SampleLimit: 100})
	if e != nil || r.Errors != 4 || r.InvalidRows != 2 || r.ValidRows != 1 {
		t.Fatalf("%+v %v", r, e)
	}
	// A multiline record spanning internal 32 KiB chunks must stay intact.
	big := "n,d,s\n1,2024-02-29,\"" + strings.Repeat("x", 40000) + "\nend\"\n"
	r, e = Validate(context.Background(), strings.NewReader(big), "csv", s, Options{})
	if e != nil || r.Rows != 1 || r.Errors != 0 {
		t.Fatalf("%+v %v", r, e)
	}
}
func BenchmarkJSONL(b *testing.B) {
	s, _ := ParseSchema([]byte(`{"columns":[{"name":"id","type":"integer","required":true}]}`))
	data := strings.Repeat("{\"id\":1}\n", 10000)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, e := Validate(context.Background(), strings.NewReader(data), "jsonl", s, Options{})
		if e != nil {
			b.Fatal(e)
		}
	}
}
