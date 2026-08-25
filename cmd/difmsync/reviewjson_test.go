package main

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mjrossi/difm-spotify-sync/internal/store/sqlite"
	"github.com/mjrossi/difm-spotify-sync/pkg/match"
)

// The keys `review --json` emits are an interface an operator can script
// against. Pinning them here means a rename shows up as a failing test rather
// than as somebody's jq pipeline breaking after an upgrade.
//
// The stronger reason is structural: the view type exists so that a column
// added to review_queue cannot publish itself. A test that only checked a few
// fields would not notice a new one arriving, so this asserts the key set
// exactly.
func TestReviewJSONKeysAreStable(t *testing.T) {
	item := sqlite.ReviewItem{
		AccountID:   7,
		DifmTrackID: 3057639,
		DifmVoteID:  99,
		Artist:      "deadmau5",
		Title:       "Strobe",
		DurationSec: 634,
		DetailsURL:  "https://api.audioaddict.com/v1/di/tracks/3057639",
		BestScore:   0.72,
		Reason:      "below_auto",
		Status:      "pending",
		LikedAt:     time.Date(2026, 8, 23, 11, 4, 18, 0, time.UTC),
		Candidates: []match.Scored{{
			Candidate: match.Candidate{
				ID: "spotify:track:x", Artist: "deadmau5",
				Title: "Strobe - Radio Edit", DurationSec: 366, ISRC: "USUS11000123",
			},
			Score: 0.72,
			Why:   "duration differs by 268s",
		}},
	}

	b, err := json.Marshal(newReviewItemJSON(item))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := []string{
		"artist", "best_score", "candidates", "details_url", "difm_track_id",
		"difm_vote_id", "duration_sec", "liked_at", "reason", "status", "title",
	}
	if diff := cmp.Diff(want, keysOf(got)); diff != "" {
		t.Errorf("item keys (-want +got):\n%s", diff)
	}

	// account_id is an internal row id that means nothing outside this
	// database, and the caller already chose the account.
	if _, leaked := got["account_id"]; leaked {
		t.Error("account_id reached the operator's JSON")
	}

	cands, ok := got["candidates"].([]any)
	if !ok || len(cands) != 1 {
		t.Fatalf("candidates = %#v, want one element", got["candidates"])
	}
	wantCand := []string{"artist", "duration_sec", "isrc", "score", "spotify_id", "title", "why"}
	if diff := cmp.Diff(wantCand, keysOf(cands[0].(map[string]any))); diff != "" {
		t.Errorf("candidate keys (-want +got):\n%s", diff)
	}

	// sqlite.TimeFormat, the same rendering the status output and the
	// watermark use. One timestamp format across the operator surface.
	if got["liked_at"] != "2026-08-23T11:04:18.000Z" {
		t.Errorf("liked_at = %v, want sqlite.TimeFormat in UTC", got["liked_at"])
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
