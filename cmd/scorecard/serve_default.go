//go:build !scorecard

package main

import "errors"

func serve([]string) error { return errors.New("serve requires a scorecard-tagged build") }
