package mdblist

import (
	"errors"
	"testing"
)

func TestPermanentClientErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&HTTPError{Status: 400}, true},
		{&HTTPError{Status: 404}, true},
		{&HTTPError{Status: 422}, true},
		{&HTTPError{Status: 401}, false},
		{&HTTPError{Status: 403}, false},
		{&HTTPError{Status: 429}, false},
		{&HTTPError{Status: 503}, false},
		{errors.New("network timeout"), false},
	} {
		if got := permanentClientError(tc.err); got != tc.want {
			t.Errorf("permanentClientError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
