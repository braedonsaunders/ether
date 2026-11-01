//go:build !scorecard

package main

import "errors"

func soak([]string) error { return errors.New("soak requires a scorecard-tagged build") }
