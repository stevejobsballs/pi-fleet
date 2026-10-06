package password

import "testing"

func BenchmarkDefault(b *testing.B) {
	v, _ := Hash("tumbleweed-gasket-42", Default)
	for i := 0; i < b.N; i++ {
		v.Check("tumbleweed-gasket-42")
	}
}
