package isureal

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type primaryReadKey struct{}
type primaryReadScope struct {
	primary *sqlx.DB
	api     *primaryAPIClient
	auth    string
	ip      string
	local   bool
	once    sync.Once
	viewer  viewerContext
	err     error
}

func primaryScope(ctx context.Context) *primaryReadScope {
	v, _ := ctx.Value(primaryReadKey{}).(*primaryReadScope)
	return v
}

// This request-local permission snapshot comes exclusively from Primary. It
// combines the former user/posting/blocked/friends/second-degree round trips.
func (s *primaryReadScope) loadViewer(ctx context.Context, id string) (viewerContext, error) {
	s.once.Do(func() {
		if s.api != nil {
			s.err = s.api.call(ctx, s.auth, s.ip, primaryRPCRequest{Op: "viewer"}, &s.viewer)
		} else {
			s.viewer, s.err = loadPrimaryViewer(ctx, s.primary, id)
		}
	})
	if s.err != nil {
		return s.viewer, s.err
	}
	if s.viewer.ID != id {
		if s.api != nil {
			return viewerContext{}, sql.ErrNoRows
		}
		return loadPrimaryViewer(ctx, s.primary, id)
	}
	return s.viewer, nil
}
func loadPrimaryViewer(ctx context.Context, db sqlx.QueryerContext, id string) (viewerContext, error) {
	var row struct {
		User
		Posted  bool            `db:"posted"`
		Blocked json.RawMessage `db:"blocked"`
		Friends json.RawMessage `db:"friends"`
		Graph   json.RawMessage `db:"graph"`
	}
	err := sqlx.GetContext(ctx, db, &row, `WITH actor AS MATERIALIZED (
 SELECT u.`+strings.Join(userColumns, ", u.")+`, EXISTS(
 SELECT FROM posts p JOIN posting_windows posted ON posted.id=p.posting_window_id
 JOIN posting_windows current ON current.region=u.region AND current.ended_at IS NULL AND current.local_date=posted.local_date
 WHERE p.author_id=u.id AND p.deleted_at IS NULL) AS posted
 FROM users u WHERE u.id=$1 AND u.status='active'
 ), friends AS MATERIALIZED (
 SELECT CASE WHEN user_low_id=$1 THEN user_high_id ELSE user_low_id END AS id FROM friendships
 WHERE (user_low_id=$1 OR user_high_id=$1) AND (SELECT posted FROM actor)
 )
 SELECT actor.*,COALESCE((SELECT jsonb_agg(CASE WHEN blocker_id=$1 THEN blocked_id ELSE blocker_id END)
 FROM block_relations WHERE (blocker_id=$1 OR blocked_id=$1) AND actor.posted),'[]'::jsonb) AS blocked,
 COALESCE((SELECT jsonb_agg(id) FROM friends),'[]'::jsonb) AS friends,
 COALESCE((SELECT jsonb_agg(jsonb_build_array(user_low_id,user_high_id)) FROM friendships
 WHERE user_low_id IN(SELECT id FROM friends) OR user_high_id IN(SELECT id FROM friends)),'[]'::jsonb) AS graph
 FROM actor`, id)
	viewer := viewerContext{User: row.User, HasPostedToday: row.Posted, Friends: map[string]bool{}, Blocked: map[string]bool{}, FriendsOfFriends: map[string]bool{}, MutualFriends: map[string]int{}}
	if err != nil {
		return viewer, err
	}
	if err = viewer.User.setImageKey(); err != nil {
		return viewer, err
	}
	var blocked, friends []string
	var graph [][2]string
	if err = json.Unmarshal(row.Blocked, &blocked); err != nil {
		return viewer, err
	}
	if err = json.Unmarshal(row.Friends, &friends); err != nil {
		return viewer, err
	}
	if err = json.Unmarshal(row.Graph, &graph); err != nil {
		return viewer, err
	}
	for _, id := range blocked {
		viewer.Blocked[id] = true
	}
	for _, id := range friends {
		viewer.Friends[id] = true
	}
	for _, pair := range graph {
		if viewer.Friends[pair[0]] {
			viewer.FriendsOfFriends[pair[1]] = true
			if pair[1] != id {
				viewer.MutualFriends[pair[1]]++
			}
		}
		if viewer.Friends[pair[1]] {
			viewer.FriendsOfFriends[pair[0]] = true
			if pair[0] != id {
				viewer.MutualFriends[pair[0]]++
			}
		}
	}
	return viewer, nil
}

// Replica payloads must never decide whether a post still exists or is visible.
// Only narrow current permission fields are fetched from Primary in one batch.
func (h *HandlerContext) authorizePosts(c *echo.Context, viewer viewerContext, posts []PostWithAuthor, feed string) ([]PostWithAuthor, error) {
	local, _ := c.Get("usingReplica").(bool)
	if !local || len(posts) == 0 {
		return posts, nil
	}
	if h.PrimaryAPI != nil {
		if authorization, ok := c.Get("primary_read_authorization").(*primaryReadAuthorization); ok && authorization.PostsKnown && feed == "" && len(posts) == 1 {
			return applyPostPermissions(posts, authorization.Posts), nil
		}
		return primaryScope(c.Request().Context()).approvePosts(c.Request().Context(), viewer, posts, feed)
	}
	return authorizePostPayloads(c.Request().Context(), h.WriteDB(), viewer, posts, feed)
}
func authorizePostPayloads(ctx context.Context, primary *sqlx.DB, viewer viewerContext, posts []PostWithAuthor, feed string) ([]PostWithAuthor, error) {
	if len(posts) == 0 {
		return posts, nil
	}

	ids := make([]string, len(posts))
	for i := range posts {
		ids[i] = posts[i].ID
	}
	var states []struct {
		Post
		Following   bool   `db:"following"`
		AccountType string `db:"account_type"`
	}
	err := primary.SelectContext(ctx, &states, `SELECT p.id,p.author_id,p.visibility,w.ended_at AS window_ended_at,u.account_type,
 EXISTS(SELECT FROM follows WHERE follower_id=$2 AND followee_id=p.author_id) AS following
 FROM posts p JOIN posting_windows w ON w.id=p.posting_window_id JOIN users u ON u.id=p.author_id AND u.status='active'
 WHERE p.id=ANY($1::text[]) AND p.deleted_at IS NULL`, ids, viewer.ID)
	if err != nil {
		return nil, err
	}
	index := map[string]int{}
	for i := range states {
		index[states[i].ID] = i
	}
	result := posts[:0]
	for _, post := range posts {
		at, ok := index[post.ID]
		if !ok {
			continue
		}
		state := states[at]
		if !viewer.canView(state.Post) {
			continue
		}
		switch feed {
		case "public":
			if state.Visibility != "public" {
				continue
			}
		case "friends":
			if !viewer.Friends[state.AuthorID] {
				continue
			}
		case "following":
			if !state.Following {
				continue
			}
		case "friends-of-friends":
			if viewer.Friends[state.AuthorID] || !viewer.FriendsOfFriends[state.AuthorID] {
				continue
			}
		}
		post.Visibility = state.Visibility
		post.WindowEndedAt = state.WindowEndedAt
		post.Author.AccountType = state.AccountType
		result = append(result, post)
	}
	return result, nil
}

func (h *HandlerContext) readPost(c *echo.Context, id string) (PostWithAuthor, error) {
	post, err := getPostWithAuthor(c.Request().Context(), h.ReadDB(c), id)
	if err != nil {
		if local, _ := c.Get("usingReplica").(bool); local {
			if h.PrimaryAPI != nil {
				var payload primaryPostPayload
				if err := h.callPrimary(c, primaryRPCRequest{Op: "post_payload", ID: id}, &payload); err != nil {
					return PostWithAuthor{}, err
				}
				payload.Author.SecretKeyHash = payload.SecretKeyHash
				if err := payload.Author.setImageKey(); err != nil {
					return PostWithAuthor{}, err
				}
				return PostWithAuthor{Post: payload.Post, Author: payload.Author}, nil
			}
			return getPostWithAuthor(c.Request().Context(), h.WriteDB(), id)
		}
	}
	return post, err
}

func activeInteractionIDs(ctx context.Context, primary *sqlx.DB, comments, reactions []string) (map[string]bool, error) {
	var ids []string
	err := primary.SelectContext(ctx, &ids, `SELECT c.id FROM comments c JOIN users u ON u.id=c.author_id AND u.status='active'
 WHERE c.id=ANY($1::text[]) AND c.deleted_at IS NULL
 UNION ALL SELECT r.id FROM realmojis r JOIN users u ON u.id=r.reactor_id AND u.status='active'
 WHERE r.id=ANY($2::text[]) AND r.deleted_at IS NULL AND r.replaced_at IS NULL`, comments, reactions)
	result := map[string]bool{}
	for _, id := range ids {
		result[id] = true
	}
	return result, err
}
func filterMemoryInteractions(ctx context.Context, primary *sqlx.DB, items []memoryPostResponse) error {
	var comments, reactions []string
	for _, item := range items {
		for _, v := range item.Comments {
			comments = append(comments, v.ID)
		}
		for _, v := range item.RealMojis {
			reactions = append(reactions, v.ID)
		}
	}
	if len(comments)+len(reactions) == 0 {
		return nil
	}
	active, err := activeInteractionIDs(ctx, primary, comments, reactions)
	if err != nil {
		return err
	}
	for i := range items {
		cs := items[i].Comments[:0]
		for _, v := range items[i].Comments {
			if active[v.ID] {
				cs = append(cs, v)
			}
		}
		items[i].Comments = cs
		rs := items[i].RealMojis[:0]
		for _, v := range items[i].RealMojis {
			if active[v.ID] {
				rs = append(rs, v)
			}
		}
		items[i].RealMojis = rs
	}
	return nil
}

type userPermissionState struct {
	ID          string `db:"id"`
	AccountType string `db:"account_type"`
	Blocked     bool   `db:"blocked"`
	Friend      bool   `db:"friend"`
}

func (h *HandlerContext) userPermissions(c *echo.Context, ids []string) (map[string]userPermissionState, error) {
	if h.PrimaryAPI != nil {
		if authorization, ok := c.Get("primary_read_authorization").(*primaryReadAuthorization); ok && authorization.Users != nil && len(ids) == 1 {
			return authorization.Users, nil
		}
		var states map[string]userPermissionState
		err := h.callPrimary(c, primaryRPCRequest{Op: "user_permissions", IDs: ids}, &states)
		return states, err
	}
	var states []userPermissionState
	err := h.WriteDB().SelectContext(c.Request().Context(), &states, `SELECT u.id,u.account_type,
 EXISTS(SELECT FROM block_relations WHERE (blocker_id=$2 AND blocked_id=u.id) OR (blocked_id=$2 AND blocker_id=u.id)) AS blocked,
 EXISTS(SELECT FROM friendships WHERE user_low_id=LEAST($2,u.id) AND user_high_id=GREATEST($2,u.id)) AS friend
 FROM users u WHERE u.id=ANY($1::text[]) AND u.status='active'`, ids, GetSession(c).UserID)
	result := map[string]userPermissionState{}
	for _, s := range states {
		result[s.ID] = s
	}
	return result, err
}
