package main_test

import (
	"strings"
	"testing"

	main "github.com/MacroPower/tfc_archiver/cmd/tfc_archiver"
)

func BenchmarkHello(b *testing.B) {
	sb := strings.Builder{}

	for range b.N {
		sb.Reset()

		err := main.Hello(&sb)
		if err != nil {
			b.Fatal(err)
		}
	}
}
