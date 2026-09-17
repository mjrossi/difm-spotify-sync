package syncer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
	"github.com/mjrossi/difm-spotify-sync/pkg/difm"
	"github.com/mjrossi/difm-spotify-sync/pkg/spotify"
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want sqlite.RunErrorKind
	}{
		{
			"revoked grant, wrapped", fmt.Errorf("spotify search: %w",
				fmt.Errorf("%w (invalid_grant): %w", spotify.ErrGrantRevoked, spotify.ErrUnauthorized)),
			sqlite.KindSpotifyGrantRevoked,
		},
		{"api 403 is not a revoked grant", fmt.Errorf("%w: status 403", spotify.ErrUnauthorized), sqlite.KindError},
		{"difm unauthorized", fmt.Errorf("fetch likes: %w", difm.ErrUnauthorized), sqlite.KindDiFMUnauthorized},
		{"spotify rate limit", &spotify.RateLimitError{RetryAfter: time.Minute, StatusCode: 429}, sqlite.KindRateLimited},
		{"difm rate limit", &difm.RateLimitError{StatusCode: 429}, sqlite.KindRateLimited},
		{"context canceled", context.Canceled, sqlite.KindError},
		{"anything else", errors.New("status 500"), sqlite.KindError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.err); got != tc.want {
				t.Errorf("classify(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
