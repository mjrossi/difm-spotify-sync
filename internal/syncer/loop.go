package syncer

import (
	"errors"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
	"github.com/mjrossi/difm-spotify-sync/pkg/difm"
	"github.com/mjrossi/difm-spotify-sync/pkg/spotify"
)

// classify maps the error a pass ended with onto the kind recorded in
// sync_runs. It lives here rather than in the store because the store
// must not import the API packages, and here rather than at each return
// site because a kind set in one branch and forgotten in another is the
// failure mode. KindIncomplete is the one kind not decided here: RunOnce
// sets it at the single site that returns ErrPassIncomplete.
func classify(err error) sqlite.RunErrorKind {
	switch {
	case errors.Is(err, spotify.ErrGrantRevoked):
		return sqlite.KindSpotifyGrantRevoked
	case errors.Is(err, difm.ErrUnauthorized):
		return sqlite.KindDiFMUnauthorized
	case errors.Is(err, spotify.ErrRateLimited), errors.Is(err, difm.ErrRateLimited):
		return sqlite.KindRateLimited
	default:
		return sqlite.KindError
	}
}
