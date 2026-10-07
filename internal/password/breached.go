package password

import (
	_ "embed"
	"sync"
)

// breachedFilter holds about 2.2 million passwords of MinLength or more
// characters published from data breaches, lower-cased, as a Bloom filter
// with a 1-in-1000 false-positive rate (NIST SP 800-63B §5.1.1.2; how it
// is built is in BREACHED.md). It works offline, on every Pi.
//
//go:embed breached.bloom
var breachedFilter []byte

var breachedSet = sync.OnceValue(func() *Bloom {
	b, err := ParseBloom(breachedFilter)
	if err != nil {
		panic(err) // built into the program: a damaged filter is a build error
	}
	return b
})

func isBreached(lower string) bool { return breachedSet().Has(lower) }
