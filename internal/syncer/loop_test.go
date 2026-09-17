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

func TestNextDelay(t *testing.T) {
	const interval = 2 * time.Hour
	for _, tc := range []struct {
		name string
		err  error
		want time.Duration
	}{
		{"clean pass", nil, interval},
		{"unrelated error", errors.New("status 500"), interval},
		{"spotify retry-after honored", &spotify.RateLimitError{RetryAfter: 5 * time.Minute}, 5 * time.Minute},
		{"difm retry-after honored", &difm.RateLimitError{RetryAfter: 10 * time.Minute}, 10 * time.Minute},
		{"below the floor is raised to it", &spotify.RateLimitError{RetryAfter: 5 * time.Second}, minInterval},
		{"above the ceiling is capped", &spotify.RateLimitError{RetryAfter: 72 * time.Hour}, maxRetryDelay},
		{"no header falls back to the interval", &spotify.RateLimitError{StatusCode: 429}, interval},
		{"wrapped by the engine", fmt.Errorf("spotify search: %w", &spotify.RateLimitError{RetryAfter: 3 * time.Minute}), 3 * time.Minute},
		{"found through a multi-%w chain", fmt.Errorf("%w: %w", ErrPassIncomplete, &difm.RateLimitError{RetryAfter: 4 * time.Minute}), 4 * time.Minute},
		{"longer than the interval is still honored", &spotify.RateLimitError{RetryAfter: 6 * time.Hour}, 6 * time.Hour},
		{"negative is treated as absent", &difm.RateLimitError{RetryAfter: -time.Second}, interval},
		{"exactly the floor", &spotify.RateLimitError{RetryAfter: minInterval}, minInterval},
		{"exactly the ceiling", &spotify.RateLimitError{RetryAfter: maxRetryDelay}, maxRetryDelay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextDelay(tc.err, interval); got != tc.want {
				t.Errorf("nextDelay(%v, %s) = %s, want %s", tc.err, interval, got, tc.want)
			}
		})
	}
}
