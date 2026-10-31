package isureal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type primaryRPCRequest struct {
	Op          string   `json:"op"`
	Route       string   `json:"route,omitempty"`
	NeedSession bool     `json:"need_session,omitempty"`
	NeedViewer  bool     `json:"need_viewer,omitempty"`
	ID          string   `json:"id,omitempty"`
	IDs         []string `json:"ids,omitempty"`
	Feed        string   `json:"feed,omitempty"`
	CommentIDs  []string `json:"comment_ids,omitempty"`
	ReactionIDs []string `json:"reaction_ids,omitempty"`
	ReplicaName string   `json:"replica_name,omitempty"`
}
type primaryRPCResult struct {
	Data   json.RawMessage `json:"data,omitempty"`
	Error  string          `json:"error,omitempty"`
	NoRows bool            `json:"no_rows,omitempty"`
}
type primaryIdentity struct {
	System   string `db:"system"`
	Timeline int    `db:"timeline"`
	LSN      string `db:"lsn"`
	Safe     bool   `db:"safe"`
}
type primaryImageAuthorization struct {
	SecretKeyHash string `db:"secret_key_hash"`
	MediaType     string `db:"media_type"`
	Visible       bool   `db:"visible"`
}
type primaryPostPermission struct {
	ID            string
	Visibility    string
	WindowEndedAt *time.Time
	AccountType   string
}
type primaryUserPayload struct {
	User          User
	SecretKeyHash string
}
type primaryPostPayload struct {
	Post          Post
	Author        User
	SecretKeyHash string
}
type primaryMemoryPermissions struct {
	Posts  []primaryPostPermission
	Active map[string]bool
}
type primaryReadAuthorization struct {
	Session          *Session
	SessionRegion    string
	Viewer           *viewerContext
	Posts            []primaryPostPermission
	PostsKnown       bool
	Users            map[string]userPermissionState
	CanComment       *bool
	CanView          *bool
	CurrentPostID    string
	CurrentPostKnown bool
	Image            *primaryImageAuthorization
	ImageKnown       bool
}

func sqlNoRows() error { return sql.ErrNoRows }
func primaryReplicaIdentity(ctx context.Context, db *sqlx.DB, name string) (primaryIdentity, error) {
	var identity primaryIdentity
	err := db.GetContext(ctx, &identity, `SELECT s.system_identifier::text AS system,c.timeline_id AS timeline,
 pg_current_wal_flush_lsn()::text AS lsn,
 NOT pg_is_in_recovery()
 AND (SELECT count(*)=1 FROM pg_stat_replication r JOIN pg_replication_slots slot ON slot.active_pid=r.pid
 WHERE (r.application_name,r.client_addr::text,slot.slot_name) IN
 (('isu02','192.168.2.10/32','isu02'),('isu03','192.168.3.10/32','isu03'))
 AND r.application_name=$1 AND r.state='streaming' AND slot.slot_type='physical'
 AND (r.replay_lsn>=pg_current_wal_flush_lsn() OR r.replay_lag<=interval '2 seconds')) AS safe
 FROM pg_control_system() s CROSS JOIN pg_control_checkpoint() c`, name)
	return identity, err
}

const currentPostIDQuery = `SELECT p.id FROM posts p
 JOIN posting_windows w ON w.id=p.posting_window_id JOIN users u ON u.id=p.author_id
 JOIN posting_windows current ON current.region=u.region AND current.ended_at IS NULL AND current.local_date=w.local_date
 WHERE p.author_id=$1 AND p.deleted_at IS NULL`

func (h *HandlerContext) currentPostID(c *echo.Context) (string, error) {
	if h.PrimaryAPI != nil {
		if value, ok := c.Get("primary_read_authorization").(*primaryReadAuthorization); ok && value.CurrentPostKnown {
			if value.CurrentPostID == "" {
				return "", sql.ErrNoRows
			}
			return value.CurrentPostID, nil
		}
		var id string
		err := h.callPrimary(c, primaryRPCRequest{Op: "current_post"}, &id)
		return id, err
	}
	var id string
	err := h.WriteDB().GetContext(c.Request().Context(), &id, currentPostIDQuery, GetSession(c).UserID)
	return id, err
}
func (h *HandlerContext) callPrimary(c *echo.Context, input primaryRPCRequest, output any) error {
	return h.PrimaryAPI.call(c.Request().Context(), c.Request().Header.Get("Authorization"), c.RealIP(), input, output)
}
func rpcPostPermissions(ctx context.Context, db *sqlx.DB, viewer viewerContext, ids []string, feed string) ([]primaryPostPermission, error) {
	dummy := make([]PostWithAuthor, len(ids))
	for i, id := range ids {
		dummy[i].ID = id
	}
	allowed, err := authorizePostPayloads(ctx, db, viewer, dummy, feed)
	if err != nil {
		return nil, err
	}
	result := make([]primaryPostPermission, 0, len(allowed))
	for _, p := range allowed {
		result = append(result, primaryPostPermission{p.ID, p.Visibility, p.WindowEndedAt, p.Author.AccountType})
	}
	return result, nil
}
func primaryReadNeedsViewer(route string) bool {
	switch route {
	case "/api/feeds/:feed", "/api/posts/:postId", "/api/posts/:postId/comments", "/api/posts/:postId/realmojis":
		return true
	}
	return false
}
func (h *HandlerContext) authenticatePrimaryRPC(c *echo.Context, required bool) error {
	if strings.HasPrefix(c.Request().Header.Get("Authorization"), "Bearer ") && required {
		if err := checkPrimarySessionAndBan(h, c); err != nil {
			return err
		}
	} else {
		banned, err := h.IPBans.Banned(c.Request().Context(), h.WriteDB(), c.RealIP())
		if err != nil {
			return err
		}
		if banned {
			return NewAPIError(403, "ip_banned")
		}
		if required {
			session, err := getSessionFromHeader(c, h.WriteDB())
			if err != nil {
				return err
			}
			c.Set("session", session)
		}
	}
	if required && GetSession(c) != nil && GetSession(c).Region != forwardedRegion(c, h.Config) {
		return NewAPIError(403, "region_mismatch")
	}
	return nil
}
func (h *HandlerContext) primaryRPCExecute(c *echo.Context, input primaryRPCRequest) (any, error) {
	ctx := c.Request().Context()
	if input.Op == "diagnostics" {
		stats := h.WriteDB().Stats()
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		return map[string]any{"db_open": stats.OpenConnections, "db_in_use": stats.InUse, "db_wait_count": stats.WaitCount, "db_wait_ms": stats.WaitDuration.Milliseconds(), "goroutines": runtime.NumGoroutine(), "heap_mib": memory.HeapAlloc >> 20}, nil
	}
	if input.Op == "ready" {
		return true, h.WriteDB().PingContext(ctx)
	}
	if input.Op == "replica_health" {
		expected := "isu02"
		if forwardedRegion(c, h.Config) == "america" {
			expected = "isu03"
		}
		if input.ReplicaName != expected {
			return nil, NewAPIError(403, "primary_api_forbidden")
		}
		return primaryReplicaIdentity(ctx, h.WriteDB(), input.ReplicaName)
	}
	required := input.Op != "image_auth" && (input.Op != "read_auth" || input.NeedSession)
	if err := h.authenticatePrimaryRPC(c, required); err != nil {
		return nil, err
	}
	if required && input.Op != "read_auth" && GetSession(c) == nil {
		return nil, NewAPIError(401, "invalid_session")
	}
	if input.Op == "read_auth" {
		result := primaryReadAuthorization{Session: GetSession(c)}
		if result.Session != nil {
			result.SessionRegion = result.Session.Region
		}
		if input.NeedSession && result.Session != nil {
			if primaryReadNeedsViewer(input.Route) {
				viewer, err := loadPrimaryViewer(ctx, h.WriteDB(), result.Session.UserID)
				if err != nil {
					return nil, err
				}
				result.Viewer = &viewer
				scope := &primaryReadScope{primary: h.WriteDB()}
				scope.once.Do(func() { scope.viewer = viewer })
				ctx = context.WithValue(ctx, primaryReadKey{}, scope)
				c.SetRequest(c.Request().WithContext(ctx))
			}
			switch input.Route {
			case "/api/posts/:postId":
				posts, err := rpcPostPermissions(ctx, h.WriteDB(), *result.Viewer, []string{input.ID}, "")
				if err != nil {
					return nil, err
				}
				result.Posts = posts
				result.PostsKnown = true
			case "/api/posts/:postId/comments":
				allowed, err := canComment(ctx, h.WriteDB(), result.Session.UserID, input.ID)
				if err != nil {
					return nil, err
				}
				result.CanComment = &allowed
			case "/api/posts/:postId/realmojis":
				allowed, err := canViewPost(ctx, h.WriteDB(), result.Session.UserID, input.ID)
				if err != nil {
					return nil, err
				}
				result.CanView = &allowed
			case "/api/posts/me/current":
				result.CurrentPostKnown = true
				err := h.WriteDB().GetContext(ctx, &result.CurrentPostID, currentPostIDQuery, result.Session.UserID)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return nil, err
				}
			case "/api/users/:userId":
				users, err := h.userPermissions(c, []string{input.ID})
				if err != nil {
					return nil, err
				}
				result.Users = users
			}
		}
		if !input.NeedSession && input.ID != "" {
			result.ImageKnown = true
			var image primaryImageAuthorization
			err := h.WriteDB().GetContext(ctx, &image, imageAccessQuery, input.ID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if err == nil {
				result.Image = &image
			}
		}
		return result, nil
	}
	userID := ""
	if GetSession(c) != nil {
		userID = GetSession(c).UserID
	}
	scope := &primaryReadScope{primary: h.WriteDB()}
	ctx = context.WithValue(ctx, primaryReadKey{}, scope)
	c.SetRequest(c.Request().WithContext(ctx))
	switch input.Op {
	case "viewer":
		return loadPrimaryViewer(ctx, h.WriteDB(), userID)
	case "authorize_posts":
		viewer, err := scope.loadViewer(ctx, userID)
		if err != nil {
			return nil, err
		}
		return rpcPostPermissions(ctx, h.WriteDB(), viewer, input.IDs, input.Feed)
	case "active_interactions":
		return activeInteractionIDs(ctx, h.WriteDB(), input.CommentIDs, input.ReactionIDs)
	case "memory_permissions":
		posts, err := rpcPostPermissions(ctx, h.WriteDB(), viewerContext{User: User{ID: userID}}, input.IDs, "")
		if err != nil {
			return nil, err
		}
		active, err := activeInteractionIDs(ctx, h.WriteDB(), input.CommentIDs, input.ReactionIDs)
		return primaryMemoryPermissions{Posts: posts, Active: active}, err
	case "user_permissions":
		return h.userPermissions(c, input.IDs)
	case "can_comment":
		return canComment(ctx, h.WriteDB(), userID, input.ID)
	case "can_view_post":
		return canViewPost(ctx, h.WriteDB(), userID, input.ID)
	case "current_post":
		var id string
		err := h.WriteDB().GetContext(ctx, &id, currentPostIDQuery, userID)
		return id, err
	case "image_auth":
		var image primaryImageAuthorization
		err := h.WriteDB().GetContext(ctx, &image, imageAccessQuery, input.ID)
		return image, err
	case "user_payload":
		user, err := getUser(ctx, h.WriteDB(), input.ID)
		if err != nil {
			return nil, err
		}
		states, err := h.userPermissions(c, []string{input.ID})
		if err != nil {
			return nil, err
		}
		state, ok := states[input.ID]
		if !ok || state.Blocked {
			return nil, sql.ErrNoRows
		}
		if input.ID != userID && state.AccountType != "certified_public" && !state.Friend {
			user.Bio = ""
		}
		return primaryUserPayload{User: user, SecretKeyHash: user.SecretKeyHash}, nil
	case "post_payload":
		post, err := getPostWithAuthor(ctx, h.WriteDB(), input.ID)
		if err != nil {
			return nil, err
		}
		viewer, err := scope.loadViewer(ctx, userID)
		if err != nil {
			return nil, err
		}
		if !viewer.canView(post.Post) {
			return nil, sql.ErrNoRows
		}
		return primaryPostPayload{Post: post.Post, Author: post.Author, SecretKeyHash: post.Author.SecretKeyHash}, nil
	}
	return nil, NewAPIError(400, "invalid_primary_operation")
}
func primaryRPCHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input primaryRPCRequest
		decoder := json.NewDecoder(c.Request().Body)
		decoder.DisallowUnknownFields()
		var value any
		var err error
		if decodeErr := decoder.Decode(&input); decodeErr != nil {
			err = NewAPIError(400, "invalid_primary_request")
		} else {
			value, err = h.primaryRPCExecute(c, input)
		}
		result := primaryRPCResult{}
		status := 200
		if errors.Is(err, sql.ErrNoRows) {
			result.NoRows = true
		} else if err != nil {
			var apiError *APIError
			if errors.As(err, &apiError) {
				status = apiError.Status
				result.Error = apiError.Code
			} else {
				status = 500
				result.Error = "primary_operation_failed"
				c.Logger().Error("Primary API operation failed", "operation", input.Op, "error", err)
			}
		} else {
			result.Data, err = json.Marshal(value)
			if err != nil {
				return err
			}
		}
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		key := primaryAPIKey(h.ImageFiles.token)
		c.Response().Header().Set(primaryResponseHeader, primaryMAC(key, "response-v1", c.Get("primary_request_nonce").(string), strconv.Itoa(status), primaryBodyHash(data)))
		return c.Blob(status, "application/json", data)
	}
}
