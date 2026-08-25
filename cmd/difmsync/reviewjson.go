package main

import (
	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
)

// `review --json` used to encode sqlite.ReviewItem directly, which published
// Go field names as JSON keys and — more to the point — made the store's row
// struct the wire format. A column added to review_queue would have appeared
// in an operator's output unasked.
//
// internal/status forbids exactly this for the HTTP endpoints, and the reason
// it has that rule is a real incident: serving []sqlite.SyncRun published
// sync_runs.error, which carried the DI.fm member id. This is stdout rather
// than an endpoint, so it was never that bug — but it is the same shape, and
// the fix is the one already proven next door: a view type in this package,
// mapped field by field, so a new column cannot publish itself.
//
// Named keys were chosen before the first release. After it they would be a
// breaking change to anyone parsing this.
type reviewItemJSON struct {
	DifmTrackID int64                 `json:"difm_track_id"`
	DifmVoteID  int64                 `json:"difm_vote_id"`
	Artist      string                `json:"artist"`
	Title       string                `json:"title"`
	DurationSec int                   `json:"duration_sec"`
	DetailsURL  string                `json:"details_url,omitempty"`
	BestScore   float64               `json:"best_score"`
	Reason      string                `json:"reason"`
	Status      string                `json:"status"`
	LikedAt     string                `json:"liked_at"`
	Candidates  []reviewCandidateJSON `json:"candidates"`
}

type reviewCandidateJSON struct {
	SpotifyID   string  `json:"spotify_id"`
	Artist      string  `json:"artist"`
	Title       string  `json:"title"`
	DurationSec int     `json:"duration_sec"`
	ISRC        string  `json:"isrc,omitempty"`
	Score       float64 `json:"score"`
	Why         string  `json:"why"`
}

// AccountID is deliberately absent: it is an internal row id that means
// nothing outside this database, and the caller already chose the account.
func newReviewItemJSON(it sqlite.ReviewItem) reviewItemJSON {
	out := reviewItemJSON{
		DifmTrackID: it.DifmTrackID,
		DifmVoteID:  it.DifmVoteID,
		Artist:      it.Artist,
		Title:       it.Title,
		DurationSec: it.DurationSec,
		DetailsURL:  it.DetailsURL,
		BestScore:   it.BestScore,
		Reason:      it.Reason,
		Status:      it.Status,
		LikedAt:     it.LikedAt.UTC().Format(sqlite.TimeFormat),
		Candidates:  make([]reviewCandidateJSON, 0, len(it.Candidates)),
	}
	for _, c := range it.Candidates {
		out.Candidates = append(out.Candidates, reviewCandidateJSON{
			SpotifyID:   c.ID,
			Artist:      c.Artist,
			Title:       c.Title,
			DurationSec: c.DurationSec,
			ISRC:        c.ISRC,
			Score:       c.Score,
			Why:         c.Why,
		})
	}
	return out
}

func newReviewItemsJSON(items []sqlite.ReviewItem) []reviewItemJSON {
	out := make([]reviewItemJSON, 0, len(items))
	for _, it := range items {
		out = append(out, newReviewItemJSON(it))
	}
	return out
}
