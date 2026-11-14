package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// IssuePersonImages uses the same admission and serve-time revalidation as
// single portrait grants, with one credit proof and one batched transaction.
func (s *ImageGrantService) IssuePersonImages(ctx context.Context, credential string, device core.Device, targets []frontend.PersonImageTarget) ([]string, error) {
	if len(targets) > 100 {
		return nil, frontend.ErrImageBatchLimit
	}
	if s.surface != "silo" && s.surface != "vela" {
		return nil, core.ErrUnauthorized
	}
	for _, target := range targets {
		img := target.Image
		if target.Item == 0 || img.Kind != frontend.ImagePerson || img.Path == "" || len(img.Path) > 1024 || len(img.Tag) > 256 || len(img.Subject) > 256 {
			return nil, core.ErrUnauthorized
		}
	}
	viewer, err := s.grants.Resolve(ctx, string(s.surface), credential)
	if err != nil {
		return nil, err
	}
	source, ok := grantHash(credential)
	if !ok {
		return nil, core.ErrUnauthorized
	}
	deviceJSON, err := json.Marshal(device)
	if err != nil || len(deviceJSON) > 4096 {
		return nil, core.ErrUnauthorized
	}
	reader, ok := s.experience().(frontend.PeopleImageBatchReader)
	if !ok {
		return nil, core.ErrNotAvailable
	}
	allowed, err := reader.AuthorizePersonImages(ctx, viewer, device, targets)
	if err != nil {
		return nil, err
	}
	if len(allowed) != len(targets) {
		return nil, core.ErrUnauthorized
	}
	secrets := make([]string, len(targets))
	rows := make([]imageBatchRow, 0, len(targets))
	for n, target := range targets {
		if !allowed[n] {
			continue
		}
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, err
		}
		secrets[n] = base64.RawURLEncoding.EncodeToString(raw[:])
		hash, _ := validImageSecret(secrets[n])
		rows = append(rows, imageBatchRow{hash: hash, item: target.Item, image: target.Image})
	}
	if len(rows) == 0 {
		return secrets, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.insertAdmitted(ctx, tx, viewer.ID, source, device.ID, string(deviceJSON), "", rows); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return secrets, nil
}

var _ frontend.PersonImageGrantBatch = (*ImageGrantService)(nil)
