// Package silo provides Silo's native profile-driven conformance protocol.
package silo

import (
	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/conform/mediabrowser"
)

// Protocol uses the shared profile transport while keeping Silo's native auth
// and discovery behavior separate from the Jellyfin and Emby dialects.
type Protocol struct{ *mediabrowser.Protocol }

var _ conform.Protocol = (*Protocol)(nil)

func Load(dir string) (*Protocol, error) {
	p, err := mediabrowser.Load("silo", dir)
	if err != nil {
		return nil, err
	}
	return &Protocol{p}, nil
}
