// Command genbloom builds internal/password/breached.bloom, the filter of
// breached passwords pi-fleet refuses. See internal/password/BREACHED.md.
//
//	go run ./internal/password/genbloom -out internal/password/breached.bloom FILE...
//
// Each FILE has one password per line. Only passwords pi-fleet could
// otherwise accept (at least password.MinLength characters) are kept,
// lower-cased, as the check is case-insensitive.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"unicode/utf8"

	"pi-fleet/internal/password"
)

func main() {
	out := flag.String("out", "", "filter file to write")
	fpr := flag.Float64("fpr", 0.001, "false-positive rate")
	flag.Parse()
	if *out == "" || flag.NArg() == 0 {
		log.Fatal("usage: genbloom -out FILE LIST...")
	}
	set := map[string]struct{}{}
	for _, f := range flag.Args() {
		fh, err := os.Open(f)
		if err != nil {
			log.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		kept, read := 0, 0
		for sc.Scan() {
			read++
			line := strings.TrimRight(sc.Text(), "\r")
			if !utf8.ValidString(line) || utf8.RuneCountInString(line) < password.MinLength {
				continue
			}
			set[strings.ToLower(line)] = struct{}{}
			kept++
		}
		if err := sc.Err(); err != nil {
			log.Fatal(err)
		}
		fh.Close()
		fmt.Fprintf(os.Stderr, "%s: %d lines, %d kept\n", f, read, kept)
	}
	n := uint64(len(set))
	m := uint64(math.Ceil(-float64(n) * math.Log(*fpr) / (math.Ln2 * math.Ln2)))
	k := uint32(math.Round(float64(m) / float64(n) * math.Ln2))
	b := password.NewBloom(m, k)
	for s := range set {
		b.Add(s)
	}
	data, _ := b.MarshalBinary()
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "%d distinct passwords, %d bits (%.1f MB), %d hashes, false-positive rate %g\n",
		n, m, float64(len(data))/1e6, k, *fpr)
}
