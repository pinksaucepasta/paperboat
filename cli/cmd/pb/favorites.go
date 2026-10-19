package main

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func compareFavorites(a, b bool) int {
	if a == b {
		return 0
	}
	if a {
		return -1
	}
	return 1
}

type favoriteSet map[string]struct{}

func (f favoriteSet) IsFavorite(kind, id string) bool {
	_, ok := f[kind+":"+id]
	return ok
}

func (f favoriteSet) Set(kind, id string, favorite bool) {
	key := kind + ":" + id
	if favorite {
		f[key] = struct{}{}
		return
	}
	delete(f, key)
}

func loadFavorites(ctx context.Context, client *api.Client) (favoriteSet, error) {
	items, err := client.ListFavorites(ctx)
	if err != nil {
		return nil, friendlyCommandError(err)
	}
	favorites := make(favoriteSet, len(items))
	for _, item := range items {
		favorites[item.Kind+":"+item.ResourceID] = struct{}{}
	}
	return favorites, nil
}

var errFavoriteToggle = errors.New("favorite toggle requested")

func setFavorite(ctx context.Context, client *api.Client, kind, id string, favorite bool) error {
	_, err := client.SetFavorite(ctx, kind, id, favorite)
	if err == nil {
		return nil
	}
	var apiErr *api.APIError
	if errors.As(err, &apiErr) && apiErr.Code == "favorite_limit_reached" {
		return errors.New("you can favorite up to five items; unfavorite another machine, session, or preview first")
	}
	return friendlyCommandError(err)
}
