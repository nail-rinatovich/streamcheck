package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"streamcheck/internal/server"
	"streamcheck/internal/validator"
	"syscall"
)

func main() { os.Exit(run()) }
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: streamcheck serve | validate --schema file --input file --format csv|jsonl")
		return 1
	}
	switch os.Args[1] {
	case "serve":
		if e := server.Run(ctx); e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 1
		}
		return 0
	case "validate":
		f := flag.NewFlagSet("validate", flag.ContinueOnError)
		schema := f.String("schema", "examples/schema.json", "schema file")
		input := f.String("input", "-", "input file or - for stdin")
		format := f.String("format", "csv", "csv or jsonl")
		max := f.Int("max-record-bytes", validator.DefaultMaxRecord, "maximum bytes per record")
		samples := f.Int("samples", 20, "max error samples (0..100)")
		if f.Parse(os.Args[2:]) != nil {
			return 1
		}
		sf, e := os.Open(*schema)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 1
		}
		defer sf.Close()
		data, e := io.ReadAll(io.LimitReader(sf, 32769))
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 1
		}
		s, e := validator.ParseSchema(data)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 1
		}
		r := os.Stdin
		if *input != "-" {
			r, e = os.Open(*input)
			if e != nil {
				fmt.Fprintln(os.Stderr, e)
				return 1
			}
			defer r.Close()
		}
		report, e := validator.Validate(ctx, r, *format, s, validator.Options{MaxRecordBytes: *max, SampleLimit: *samples})
		if json.NewEncoder(os.Stdout).Encode(report) != nil {
			return 1
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		fmt.Fprintf(os.Stderr, "heap_inuse_bytes=%d heap_sys_bytes=%d\n", m.HeapInuse, m.HeapSys)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 1
		}
		if report.InvalidRows > 0 {
			return 2
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "unknown command")
		return 1
	}
}
