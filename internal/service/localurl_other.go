//go:build !windows

package service

import "errors"

// PublishLocalURL is unnecessary where the tray can read the server config.
func PublishLocalURL(dataDir, baseURL string) error { return nil }

// ReadLocalURL falls back to read-only config discovery on these platforms.
func ReadLocalURL(dataDir string) (string, error) { return "", errors.ErrUnsupported }
