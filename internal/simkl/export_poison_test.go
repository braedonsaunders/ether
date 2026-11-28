package simkl

import (
	"errors"
	"testing"
)

func TestPermanentClientErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&APIError{Status: 400}, true},
		{&APIError{Status: 404}, true},
		{&APIError{Status: 422}, true},
		{&APIError{Status: 401}, false},
		{&APIError{Status: 403}, false},
		{&APIError{Status: 429}, false},
		{&APIError{Status: 500}, false},
		{&HTTPError{Status: 404}, true},
		{&HTTPError{Status: 429}, false},
		{errors.New("network timeout"), false},
	} {
		if got := permanentClientError(tc.err); got != tc.want {
			t.Errorf("permanentClientError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
