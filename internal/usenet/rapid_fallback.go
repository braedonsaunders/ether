//go:build !(!cgo && goexperiment.simd && (amd64 || arm64) && !(windows && arm64))

package usenet

import "github.com/braedonsaunders/ether/internal/yenc"

func decodeBody(br *BodyReader, opt *yenc.DecodeOptions) (*yenc.Part, error) {
	return yenc.DecodeLines(br, opt)
}
