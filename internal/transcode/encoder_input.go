package transcode

import (
	"encoding/hex"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
)

func encoderScopeHeaders(input core.EncoderInputLease, url string) (map[string]string, error) {
	if input == nil {
		return nil, nil
	}
	headers, err := input.HeadersForInput(url)
	if err != nil {
		return nil, core.ErrEncoderInputUnverified
	}
	value := headers[core.EncoderInputScopeHeader]
	if len(headers) != 1 || len(value) != 64 {
		return nil, core.ErrEncoderInputUnverified
	}
	if _, err := hex.DecodeString(value); err != nil {
		return nil, core.ErrEncoderInputUnverified
	}
	return map[string]string{core.EncoderInputScopeHeader: value}, nil
}
func (s runSpec) commandHeaders() map[string]string {
	if len(s.InputScopeHeaders) == 0 {
		return s.InputHeaders
	}
	headers := make(map[string]string, len(s.InputHeaders)+len(s.InputScopeHeaders))
	for k, v := range s.InputHeaders {
		headers[k] = v
	}
	for k, v := range s.InputScopeHeaders {
		headers[k] = v
	}
	return headers
}

func (s *Session) encoderSampleArgs(input string, args []string) ([]string, error) {
	headers, err := encoderScopeHeaders(s.inputLease, input)
	if err != nil || len(headers) == 0 {
		return args, err
	}
	for i, arg := range args {
		if arg == "-i" {
			out := append([]string(nil), args[:i]...)
			out = appendInputHeaders(out, headers)
			return append(out, args[i:]...), nil
		}
	}
	return nil, errors.New("transcode: sample input missing")
}

// Preserve error identity without rendering command secrets in diagnostics.
type scopedCommandError struct {
	cause   error
	message string
}

func (e scopedCommandError) Error() string    { return e.message }
func (e scopedCommandError) Unwrap() error    { return e.cause }
func (e scopedCommandError) GoString() string { return e.message }
func redactScopedCommandError(err error, spec runSpec) error {
	if err == nil || len(spec.InputScopeHeaders) == 0 {
		return err
	}
	return scopedCommandError{cause: err, message: redactRTXLine(err.Error(), spec.Input, spec.commandHeaders())}
}
