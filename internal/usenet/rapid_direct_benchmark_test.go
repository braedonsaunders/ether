//go:build !cgo && goexperiment.simd && (amd64 || arm64) && !(windows && arm64)

package usenet

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/mnightingale/rapidyenc"
)

// Diagnostic ceiling: public RapidYenc without Ether's guard or BodyReader.
// This is not a production-compatible replacement for the bounded adapter.
func BenchmarkRapidDirect(b *testing.B) {
	for _, line := range []int{128, 256} {
		for _, size := range []int{384 << 10, 768 << 10} {
			wire, data := benchmarkBodyFixture(b, size, line)
			for _, reuse := range []bool{false, true} {
				b.Run(fmt.Sprintf("line%d/%dKiB/reuse%v", line, size>>10, reuse), func(b *testing.B) {
					input := bytes.NewReader(wire)
					var output []byte
					if reuse {
						output = make([]byte, 0, size+64)
					}
					d := rapidyenc.NewDecoder(input, rapidyenc.WithStatusLineAlreadyRead(), rapidyenc.WithDataFunc(func() []byte { return output }))
					p, err := d.Next()
					if err != nil {
						b.Fatal(err)
					}
					if !bytes.Equal(p.Data, data) {
						b.Fatal("decoded payload differs")
					}
					b.SetBytes(int64(size))
					b.ReportAllocs()
					finish := benchmarkStartCPU(b, size)
					defer finish()
					for i := 0; i < b.N; i++ {
						input.Reset(wire)
						if _, err := d.Next(); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}
