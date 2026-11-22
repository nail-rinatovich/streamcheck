// datagen emits repeatable fixtures without retaining them in memory.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	format := flag.String("format", "csv", "csv|jsonl")
	size := flag.Int64("bytes", 256<<20, "minimum output size in bytes")
	output := flag.String("output", "-", "output path or -")
	every := flag.Int64("invalid-every", 1000, "one invalid row every N rows; 0 disables")
	padding := flag.Int("padding", 1024, "email padding bytes (1..65536)")
	flag.Parse()
	if (*format != "csv" && *format != "jsonl") || *size < 1 || *every < 0 || *padding < 1 || *padding > 65536 {
		fmt.Fprintln(os.Stderr, "invalid generator options")
		os.Exit(1)
	}
	f := os.Stdout
	if *output != "-" {
		var e error
		f, e = os.Create(*output)
		if e != nil {
			panic(e)
		}
		defer f.Close()
	}
	w := bufio.NewWriterSize(f, 256<<10)
	pad := strings.Repeat("x", *padding)
	var good, bad, header string
	if *format == "csv" {
		header = "id,email,age,active\n"
		good = "1," + pad + "@example.com,29,true\n"
		bad = "1," + pad + "@example.com,999,true\n"
	} else {
		good = `{"id":1,"email":"` + pad + `@example.com","age":29,"active":true}` + "\n"
		bad = `{"id":1,"email":"` + pad + `@example.com","age":999,"active":true}` + "\n"
	}
	n, e := w.WriteString(header)
	if e != nil {
		panic(e)
	}
	total := int64(n)
	var rows int64
	for total < *size {
		rows++
		row := good
		if *every > 0 && rows%*every == 0 {
			row = bad
		}
		n, e = w.WriteString(row)
		if e != nil {
			panic(e)
		}
		total += int64(n)
	}
	if e = w.Flush(); e != nil {
		panic(e)
	}
	fmt.Fprintf(os.Stderr, "bytes=%d rows=%d invalid_rows=%d\n", total, rows, func() int64 {
		if *every == 0 {
			return 0
		}
		return rows / *every
	}())
}
