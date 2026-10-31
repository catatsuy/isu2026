package isureal

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Compare the optimized query with the original Go authorization and ranking,
// using real PostgreSQL semantics (arrays, tuple cursors and LIMIT).
func TestFeedSQLPostgres(t *testing.T) {
	db := localPostgresTestDB(t)
	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`CREATE TEMP TABLE users (id text PRIMARY KEY, username text DEFAULT '', secret_key_hash text, display_name text DEFAULT '', bio text DEFAULT '', profile_image_id text, region text DEFAULT 'asia', account_type text DEFAULT 'personal', status text DEFAULT 'active') ON COMMIT DROP`,
		`CREATE TEMP TABLE posting_windows (id text PRIMARY KEY, on_time_deadline timestamptz, ended_at timestamptz) ON COMMIT DROP`,
		`CREATE TEMP TABLE posts (id text PRIMARY KEY, author_id text, posting_window_id text DEFAULT 'open', image_id text DEFAULT 'image', caption text DEFAULT '', visibility text, location_kind text DEFAULT 'none', latitude float8, longitude float8, locality text, posted_at timestamptz, deleted_at timestamptz) ON COMMIT DROP`,
		`CREATE TEMP TABLE follows (follower_id text, followee_id text) ON COMMIT DROP`,
		`CREATE TEMP TABLE friendships (user_low_id text, user_high_id text) ON COMMIT DROP`,
		`CREATE TEMP TABLE realmojis (post_id text, reactor_id text, replaced_at timestamptz, deleted_at timestamptz) ON COMMIT DROP`,
		`CREATE TEMP TABLE comments (post_id text, author_id text, deleted_at timestamptz) ON COMMIT DROP`,
		`INSERT INTO posting_windows VALUES ('open',now(),NULL), ('closed',now(),now())`,
	} {
		exec(q)
	}
	viewer := viewerContext{User: User{ID: "viewer"}, HasPostedToday: true,
		Friends:          map[string]bool{"friend": true, "blocked-friend": true},
		FriendsOfFriends: map[string]bool{"fof": true, "blocked-fof": true, "friend": true},
		Blocked:          map[string]bool{"blocked-friend": true, "blocked-fof": true, "blocked-stranger": true},
		MutualFriends:    map[string]int{"fof": 2, "blocked-fof": 1}}
	authors := []string{"viewer", "friend", "fof", "stranger", "blocked-friend", "blocked-fof", "blocked-stranger", "inactive"}
	for _, author := range authors {
		exec(`INSERT INTO users (id,secret_key_hash) VALUES ($1,$2)`, author, strings.Repeat("00", 32))
		exec(`INSERT INTO follows VALUES ('viewer',$1)`, author)
	}
	exec(`UPDATE users SET status='deleted' WHERE id='inactive'`)
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Equal timestamps exercise the ID tie break, including at page boundaries.
	for i := 0; i < 160; i++ {
		for _, author := range authors {
			for _, visibility := range []string{"public", "friends", "friends_of_friends", "private"} {
				id := fmt.Sprintf("%03d-%s-%s", i, author, visibility)
				exec(`INSERT INTO posts (id,author_id,visibility,posted_at) VALUES ($1,$2,$3,$4)`, id, author, visibility, stamp.Add(time.Duration(i/20)*time.Second))
			}
		}
	}
	exec(`INSERT INTO posts (id,author_id,visibility,posted_at,posting_window_id) VALUES ('closed','friend','public',now(),'closed')`)
	exec(`INSERT INTO posts (id,author_id,visibility,posted_at,deleted_at) VALUES ('deleted','friend','public',now(),now())`)
	exec(`INSERT INTO friendships VALUES ('friend','fof')`)
	// An old post outside the newest 51 candidates must still win the ranking.
	exec(`INSERT INTO comments VALUES ('000-fof-public','friend',NULL)`)
	ctx := context.Background()
	for _, feed := range []string{"friends", "public", "following", "friends-of-friends"} {
		t.Run(feed, func(t *testing.T) {
			input := getFeedRequest{Feed: feed}
			seen := map[string]bool{}
			for page := 0; page < 30; page++ {
				before, id := input.BeforePostedAt, input.BeforeID
				if feed == "friends-of-friends" {
					before = nil
					id = ""
				}
				legacy, err := selectPostsWithAuthor(ctx, tx, `AND p.author_id <> $1 AND w.ended_at IS NULL AND ($2::timestamptz IS NULL OR (p.posted_at,p.id)<($2,$3)) ORDER BY p.posted_at DESC,p.id DESC`, viewer.ID, before, id)
				if err != nil {
					t.Fatal(err)
				}
				candidates := []PostWithAuthor{}
				following := map[string]bool{}
				for _, a := range authors {
					following[a] = true
				}
				for _, p := range legacy {
					if viewer.canView(p.Post) && matchesFeed(viewer, following, p.Post, feed) {
						candidates = append(candidates, p)
					}
				}
				optimized, err := selectFeedPosts(ctx, tx, viewer, input)
				if err != nil {
					t.Fatal(err)
				}
				if feed != "friends-of-friends" && len(optimized) != min(51, len(candidates)) {
					t.Fatalf("fetched %d rows for %d eligible", len(optimized), len(candidates))
				}
				var oldScores, newScores map[string]int64
				if feed == "friends-of-friends" {
					oldScores, err = recommendationScores(ctx, tx, viewer, candidates)
					if err != nil {
						t.Fatal(err)
					}
					newScores, err = recommendationScores(ctx, tx, viewer, optimized)
					if err != nil {
						t.Fatal(err)
					}
				}
				want := makeFeedResponse(candidates, oldScores, input)
				got := makeFeedResponse(optimized, newScores, input)
				if got.HasMore != want.HasMore || !reflect.DeepEqual(feedOrder(got), feedOrder(want)) {
					t.Fatalf("page %d differs from original", page)
				}
				if page == 0 && !got.HasMore {
					t.Fatal("expected more than 50 eligible posts")
				}
				if page == 0 && feed == "friends-of-friends" && got.Items[0].ID != "000-fof-public" {
					t.Fatal("old high-score post lost")
				}
				for _, item := range got.Items {
					if seen[item.ID] {
						t.Fatalf("duplicate %s", item.ID)
					}
					seen[item.ID] = true
				}
				if !got.HasMore {
					return
				}
				last := got.Items[len(got.Items)-1]
				input.BeforeID = last.ID
				input.BeforePostedAt = &last.PostedAt
				input.BeforeScore = last.Score
			}
			t.Fatal("pagination did not finish")
		})
	}
	viewer.HasPostedToday = false
	posts, err := selectFeedPosts(ctx, tx, viewer, getFeedRequest{Feed: "public"})
	if err != nil || len(posts) != 0 {
		t.Fatalf("unposted viewer: %v %v", posts, err)
	}
}

func TestFeedResponsePageSize(t *testing.T) {
	for _, n := range []int{0, 49, 50, 51} {
		posts := make([]PostWithAuthor, n)
		got := makeFeedResponse(posts, nil, getFeedRequest{Feed: "public"})
		if len(got.Items) != min(n, 50) || got.HasMore != (n > 50) {
			t.Fatalf("size %d: %+v", n, got)
		}
	}
}

func feedOrder(response getFeedResponse) []string {
	result := make([]string, 0, len(response.Items))
	for _, item := range response.Items {
		value := item.ID
		if item.Score != nil {
			value += fmt.Sprintf(":%d", *item.Score)
		}
		result = append(result, value)
	}
	return result
}

func TestRankedFeedOrderAndCursor(t *testing.T) {
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	posts := make([]PostWithAuthor, 80)
	scores := map[string]int64{}
	for i := range posts {
		id := fmt.Sprintf("%03d", 80-i)
		posts[i].Post = Post{ID: id, PostedAt: stamp}
		scores[id] = 1
	}
	scores["001"] = 100 // The last SQL candidate wins; never limit before scoring.
	first := makeFeedResponse(posts, scores, getFeedRequest{Feed: "friends-of-friends"})
	if !first.HasMore || first.Items[0].ID != "001" || first.Items[1].ID != "080" || first.Items[49].ID != "032" {
		t.Fatalf("wrong first page: %v", feedOrder(first))
	}
	last := first.Items[49]
	second := makeFeedResponse(posts, scores, getFeedRequest{Feed: "friends-of-friends", BeforeID: last.ID, BeforePostedAt: &last.PostedAt, BeforeScore: last.Score})
	if second.HasMore || len(second.Items) != 30 || second.Items[0].ID != "031" || second.Items[29].ID != "002" {
		t.Fatalf("wrong second page: %v", feedOrder(second))
	}
	// At equal scores, timestamps take precedence over IDs.
	older := Post{ID: "zzz", PostedAt: stamp.Add(-time.Second)}
	if !rankedBefore(1, older, 1, stamp, "032") || rankedBefore(1, Post{ID: "032", PostedAt: stamp}, 1, stamp, "032") {
		t.Fatal("incorrect timestamp or exclusive cursor boundary")
	}
}
