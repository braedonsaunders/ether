package usenet

import (
	"bufio"
	"bytes"
	"fmt"
	"sync"
	"testing"

	"github.com/braedonsaunders/ether/internal/yenc"
)

type bodyBenchmarkReader struct {
	input *bytes.Reader
	conn  *Conn
	opt   *yenc.DecodeOptions
	swar  bool
	wire  []byte
}

func newBodyBenchmarkReader(wire []byte, size int, buffer string, swar bool) *bodyBenchmarkReader {
	input := bytes.NewReader(wire)
	r := &bodyBenchmarkReader{input: input, conn: &Conn{r: bufio.NewReaderSize(input, readBufferSize)}, swar: swar, wire: wire}
	if buffer != "fresh" {
		capacity := size
		if buffer == "headroom" {
			capacity += 64
		}
		r.opt = &yenc.DecodeOptions{Buffer: make([]byte, 0, capacity)}
	}
	return r
}

func (r *bodyBenchmarkReader) decode() (*yenc.Part, error) {
	r.input.Reset(r.wire)
	r.conn.r.Reset(r.input)
	br := &BodyReader{c: r.conn, end: benchmarkBodyEnd}
	var p *yenc.Part
	var err error
	if r.swar {
		p, err = yenc.DecodeLines(br, r.opt)
	} else {
		p, err = decodeBody(br, r.opt)
	}
	if err != nil {
		return p, err
	}
	return p, br.Close()
}

func benchmarkBodyEnd(err error) error { return err }

func benchmarkBodyFixture(b *testing.B, size, line int) ([]byte, []byte) {
	b.Helper()
	data := randData(95, size)
	article, err := yenc.EncodeBytes(data, yenc.EncodeOptions{Name: "bench.bin", LineLength: line})
	if err != nil {
		b.Fatal(err)
	}
	return append(article, []byte(".\r\n")...), data
}

func benchmarkCheckBody(b *testing.B, r *bodyBenchmarkReader, data []byte) {
	b.Helper()
	p, err := r.decode()
	if err != nil {
		b.Fatal(err)
	}
	if !bytes.Equal(p.Data, data) {
		b.Fatal("decoded payload differs")
	}
}

// Both implementations include BodyReader, framing, size limits and CRC.
func BenchmarkBodyDecode(b *testing.B) {
	for _, line := range []int{128, 256} {
		for _, size := range []int{384 << 10, 768 << 10} {
			wire, data := benchmarkBodyFixture(b, size, line)
			for _, buffer := range []string{"fresh", "exact", "headroom"} {
				for _, swar := range []bool{false, true} {
					b.Run(fmt.Sprintf("line%d/%dKiB/buffer%s/SWAR%v", line, size>>10, buffer, swar), func(b *testing.B) {
						r := newBodyBenchmarkReader(wire, size, buffer, swar)
						benchmarkCheckBody(b, r, data)
						b.SetBytes(int64(size))
						b.ReportAllocs()
						finish := benchmarkStartCPU(b, size)
						defer finish()
						for i := 0; i < b.N; i++ {
							if _, err := r.decode(); err != nil {
								b.Fatal(err)
							}
						}
					})
				}
			}
		}
	}
}

// Saturated independent readers, fixed at GOMAXPROCS=8 by the run command.
// Each owns its input and optional output buffer. Launch/join are measured.
func BenchmarkBodyDecodeConcurrent(b *testing.B) {
	const size = 768 << 10
	wire, data := benchmarkBodyFixture(b, size, 128)
	for _, buffer := range []string{"fresh", "exact"} {
		for _, workers := range []int{1, 4, 8, 16} {
			for _, swar := range []bool{false, true} {
				b.Run(fmt.Sprintf("buffer%s/workers%d/SWAR%v", buffer, workers, swar), func(b *testing.B) {
					readers := make([]*bodyBenchmarkReader, workers)
					for i := range readers {
						readers[i] = newBodyBenchmarkReader(wire, size, buffer, swar)
						benchmarkCheckBody(b, readers[i], data)
					}
					errs := make(chan error, workers)
					b.SetBytes(int64(size))
					b.ReportAllocs()
					finish := benchmarkStartCPU(b, size)
					var wg sync.WaitGroup
					for i, r := range readers {
						count := b.N / workers
						if i < b.N%workers {
							count++
						}
						wg.Add(1)
						go func(r *bodyBenchmarkReader, n int) {
							defer wg.Done()
							for i := 0; i < n; i++ {
								if _, err := r.decode(); err != nil {
									errs <- err
									return
								}
							}
						}(r, count)
					}
					wg.Wait()
					finish()
					close(errs)
					for err := range errs {
						b.Fatal(err)
					}
				})
			}
		}
	}
}
