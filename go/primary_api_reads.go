package isureal

import (
	"context"
	"database/sql"
	"encoding/base64"
	"net/url"
	"strconv"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

func (h *HandlerContext) authenticateViaAPI(c *echo.Context) error {
	route := c.Path()
	image := route == "/api/images/:imageId"
	input := primaryRPCRequest{Op: "read_auth", Route: route, NeedSession: !image, NeedViewer: primaryReadNeedsViewer(route)}
	if image {
		input.ID = primaryImageAuthID(c)
	} else if c.Param("postId") != "" {
		input.ID = c.Param("postId")
	} else {
		input.ID = c.Param("userId")
	}
	var authorization primaryReadAuthorization
	if err := h.callPrimary(c, input, &authorization); err != nil {
		return err
	}
	if authorization.Session != nil {
		authorization.Session.Region = authorization.SessionRegion
	}
	c.Set("session", authorization.Session)
	c.Set("primary_session_checked", true)
	c.Set("primary_read_authorization", &authorization)
	if scope := primaryScope(c.Request().Context()); scope != nil && authorization.Viewer != nil {
		scope.once.Do(func() { scope.viewer = *authorization.Viewer })
	}
	return nil
}
func primaryImageAuthID(c *echo.Context) string {
	query, err := url.ParseQuery(c.Request().URL.RawQuery)
	if err != nil || len(query["exp"]) != 1 || len(query["sig"]) != 1 {
		return ""
	}
	value := query.Get("exp")
	exp, err := strconv.ParseInt(value, 10, 64)
	if err != nil || exp <= time.Now().Unix() || strconv.FormatInt(exp, 10) != value {
		return ""
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(query.Get("sig"))
	if err != nil || len(sig) != 32 {
		return ""
	}
	return c.Param("imageId")
}
func (h *HandlerContext) imageAuthorization(c *echo.Context, id string, refresh bool) (primaryImageAuthorization, error) {
	var image primaryImageAuthorization
	if h.PrimaryAPI != nil {
		if !refresh {
			if authorization, ok := c.Get("primary_read_authorization").(*primaryReadAuthorization); ok && authorization.ImageKnown {
				if authorization.Image == nil {
					return image, sql.ErrNoRows
				}
				return *authorization.Image, nil
			}
		}
		err := h.callPrimary(c, primaryRPCRequest{Op: "image_auth", ID: id}, &image)
		return image, err
	}
	err := h.ReadDB(c).GetContext(c.Request().Context(), &image, imageAccessQuery, id)
	return image, err
}
func (h *HandlerContext) canCommentRead(c *echo.Context, id string) (bool, error) {
	if h.PrimaryAPI != nil {
		if authorization, ok := c.Get("primary_read_authorization").(*primaryReadAuthorization); ok && authorization.CanComment != nil {
			return *authorization.CanComment, nil
		}
		var allowed bool
		err := h.callPrimary(c, primaryRPCRequest{Op: "can_comment", ID: id}, &allowed)
		return allowed, err
	}
	return canComment(c.Request().Context(), h.WriteDB(), GetSession(c).UserID, id)
}
func (h *HandlerContext) canViewPostRead(c *echo.Context, id string) (bool, error) {
	if h.PrimaryAPI != nil {
		if authorization, ok := c.Get("primary_read_authorization").(*primaryReadAuthorization); ok && authorization.CanView != nil {
			return *authorization.CanView, nil
		}
		var allowed bool
		err := h.callPrimary(c, primaryRPCRequest{Op: "can_view_post", ID: id}, &allowed)
		return allowed, err
	}
	return canViewPost(c.Request().Context(), h.WriteDB(), GetSession(c).UserID, id)
}
func applyPostPermissions(posts []PostWithAuthor, permissions []primaryPostPermission) []PostWithAuthor {
	index := map[string]primaryPostPermission{}
	for _, p := range permissions {
		index[p.ID] = p
	}
	result := posts[:0]
	for _, post := range posts {
		if p, ok := index[post.ID]; ok {
			post.Visibility = p.Visibility
			post.WindowEndedAt = p.WindowEndedAt
			post.Author.AccountType = p.AccountType
			result = append(result, post)
		}
	}
	return result
}
func (s *primaryReadScope) approvePosts(ctx context.Context, viewer viewerContext, posts []PostWithAuthor, feed string) ([]PostWithAuthor, error) {
	if s.api == nil {
		return authorizePostPayloads(ctx, s.primary, viewer, posts, feed)
	}
	if len(posts) == 0 {
		return posts, nil
	}
	ids := make([]string, len(posts))
	for i, p := range posts {
		ids[i] = p.ID
	}
	var allowed []primaryPostPermission
	err := s.api.call(ctx, s.auth, s.ip, primaryRPCRequest{Op: "authorize_posts", IDs: ids, Feed: feed}, &allowed)
	if err != nil {
		return nil, err
	}
	return applyPostPermissions(posts, allowed), nil
}
func (s *primaryReadScope) activeIDs(ctx context.Context, comments, reactions []string) (map[string]bool, error) {
	if s.api == nil {
		return activeInteractionIDs(ctx, s.primary, comments, reactions)
	}
	var result map[string]bool
	err := s.api.call(ctx, s.auth, s.ip, primaryRPCRequest{Op: "active_interactions", CommentIDs: comments, ReactionIDs: reactions}, &result)
	return result, err
}
func (h *HandlerContext) activeReadIDs(c *echo.Context, comments, reactions []string) (map[string]bool, error) {
	if scope := primaryScope(c.Request().Context()); scope != nil {
		return scope.activeIDs(c.Request().Context(), comments, reactions)
	}
	return activeInteractionIDs(c.Request().Context(), h.WriteDB(), comments, reactions)
}
func memoriesViaAPI(ctx context.Context, scope *primaryReadScope, db sqlx.QueryerContext, posts []PostWithAuthor) (getMemoriesResponse, error) {
	response := getMemoriesResponse{Items: []memoryPostResponse{}}
	items := make([]memoryPostResponse, 0, len(posts))
	ids := make([]string, 0, len(posts))
	for _, p := range posts {
		ids = append(ids, p.ID)
		items = append(items, memoryPostResponse{createPostResponse: newPostResponse(p.Post, p.Author), RealMojis: []replaceRealMojiResponse{}, Comments: []createCommentResponse{}})
	}
	if len(items) == 0 {
		return response, nil
	}
	if err := loadMemoryInteractions(ctx, db, items); err != nil {
		return response, err
	}
	var commentIDs, reactionIDs []string
	for _, item := range items {
		for _, v := range item.Comments {
			commentIDs = append(commentIDs, v.ID)
		}
		for _, v := range item.RealMojis {
			reactionIDs = append(reactionIDs, v.ID)
		}
	}
	var permissions primaryMemoryPermissions
	if err := scope.api.call(ctx, scope.auth, scope.ip, primaryRPCRequest{Op: "memory_permissions", IDs: ids, CommentIDs: commentIDs, ReactionIDs: reactionIDs}, &permissions); err != nil {
		return response, err
	}
	allowed := map[string]primaryPostPermission{}
	for _, p := range permissions.Posts {
		allowed[p.ID] = p
	}
	filtered := items[:0]
	for _, item := range items {
		state, ok := allowed[item.ID]
		if !ok {
			continue
		}
		item.Visibility = state.Visibility
		item.Author.AccountType = state.AccountType
		comments := item.Comments[:0]
		for _, v := range item.Comments {
			if permissions.Active[v.ID] {
				comments = append(comments, v)
			}
		}
		item.Comments = comments
		reactions := item.RealMojis[:0]
		for _, v := range item.RealMojis {
			if permissions.Active[v.ID] {
				reactions = append(reactions, v)
			}
		}
		item.RealMojis = reactions
		filtered = append(filtered, item)
	}
	response.HasMore = len(filtered) > 50
	if response.HasMore {
		filtered = filtered[:50]
	}
	response.Items = filtered
	return response, nil
}
