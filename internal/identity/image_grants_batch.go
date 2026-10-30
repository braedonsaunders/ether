package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

const maxImageBatchItems = 50
const maxImageBatchInput = 256
const imageBatchInsertChunk = 40

type imageBatchRow struct {
	hash  string
	item  core.RatingKey
	image frontend.Image
}

// IssueBatch returns at most one primary descriptor of each requested kind per
// visible item. Source membership and each item's policy are rechecked before
// inserting independent capabilities in a single transaction.
func (s *ImageGrantService) IssueBatch(ctx context.Context, req frontend.ImageBatchRequest) ([]frontend.ImageBatchItem, error) {
	if len(req.Items) > maxImageBatchInput || len(req.Kinds) > 6 {
		return nil, frontend.ErrImageBatchLimit
	}
	keys := make([]core.RatingKey, 0, len(req.Items))
	seen := make(map[core.RatingKey]bool, len(req.Items))
	for _, key := range req.Items {
		if key == 0 {
			return nil, frontend.ErrImageBatchLimit
		}
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	if len(keys) > maxImageBatchItems {
		return nil, frontend.ErrImageBatchLimit
	}
	kinds := req.Kinds
	if len(kinds) == 0 {
		kinds = []string{frontend.ImagePoster}
	}
	wanted := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		switch kind {
		case frontend.ImagePoster, frontend.ImageBackdrop, frontend.ImageLogo, frontend.ImageThumb, frontend.ImageBanner, frontend.ImageStill:
			wanted[kind] = true
		default:
			return nil, frontend.ErrImageBatchLimit
		}
	}
	viewer, err := s.grants.Resolve(ctx, string(s.surface), req.Credential)
	if err != nil {
		return nil, err
	}
	source, ok := grantHash(req.Credential)
	if !ok {
		return nil, core.ErrUnauthorized
	}
	device, err := json.Marshal(req.Device)
	if err != nil || len(device) > 4096 {
		return nil, core.ErrUnauthorized
	}
	experience := s.experience()
	if experience == nil {
		return nil, core.ErrNotAvailable
	}
	var rows []imageBatchRow
	out := make([]frontend.ImageBatchItem, 0, len(keys))
	for _, key := range keys {
		var images []frontend.Image
		var complete bool
		if scoped, ok := experience.(frontend.ItemArtwork); ok {
			images, complete, err = scoped.ItemArtwork(ctx, viewer, req.Device, key)
		} else {
			var detail frontend.ItemDetail
			detail, err = experience.Item(ctx, frontend.ItemRequest{Viewer: viewer, Device: req.Device, Key: key})
			images, complete = detail.Images, detail.ImagesAuthoritative
		}
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrUnauthorized) {
			continue
		}
		if errors.Is(err, core.ErrNotAvailable) {
			out = append(out, frontend.ImageBatchItem{Item: key})
			continue
		}
		if err != nil {
			return nil, err
		}
		item := frontend.ImageBatchItem{Item: key, Authoritative: complete}
		if complete {
			chosen := make(map[string]bool, len(wanted))
			for _, image := range images {
				if !wanted[image.Kind] || chosen[image.Kind] {
					continue
				}
				if image.Path == "" || len(image.Path) > 1024 || len(image.Kind) > 128 || len(image.Tag) > 256 || len(image.Subject) > 256 {
					return nil, core.ErrNotAvailable
				}
				var raw [32]byte
				if _, err := rand.Read(raw[:]); err != nil {
					return nil, err
				}
				secret := base64.RawURLEncoding.EncodeToString(raw[:])
				hash, _ := validImageSecret(secret)
				item.Images = append(item.Images, frontend.GrantedImage{Image: image, Secret: secret})
				rows = append(rows, imageBatchRow{hash: hash, item: key, image: image})
				chosen[image.Kind] = true
			}
		}
		out = append(out, item)
	}
	if len(rows) == 0 {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.insertMirrored(ctx, tx, viewer.ID, source, req.Device.ID, string(device), "", rows); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

var _ frontend.ImageGrantBatch = (*ImageGrantService)(nil)
