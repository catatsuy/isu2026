package isureal

import (
	"database/sql"
	"errors"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type createPostRequest struct {
	Image        *multipart.FileHeader `form:"image" json:"-"`
	Caption      string                `form:"caption" json:"caption"`
	Visibility   string                `form:"visibility" json:"visibility"`
	LocationKind string                `form:"location_kind" json:"locationKind"`
	Latitude     *float64              `form:"latitude" json:"latitude"`
	Longitude    *float64              `form:"longitude" json:"longitude"`
	Locality     *string               `form:"locality" json:"locality"`
}

type postAuthorResponse struct {
	ID              string  `json:"id"`
	Username        string  `json:"username"`
	DisplayName     string  `json:"displayName"`
	ProfileImageURL *string `json:"profileImageUrl"`
	Bio             string  `json:"bio"`
	AccountType     string  `json:"accountType"`
}

type createPostResponse struct {
	ID              string             `json:"id"`
	Author          postAuthorResponse `json:"author"`
	PostingWindowID string             `json:"postingWindowId"`
	ImageURL        string             `json:"imageUrl"`
	Caption         string             `json:"caption"`
	Visibility      string             `json:"visibility"`
	Location        PostLocation       `json:"location"`
	PostedAt        time.Time          `json:"postedAt"`
	OnTime          bool               `json:"onTime"`
	LateSeconds     int64              `json:"lateSeconds"`
}

func createPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createPostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if input.Image == nil || !validUserText(input.Caption, 0, 1000) {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		switch input.Visibility {
		case "friends", "friends_of_friends", "public":
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		location := PostLocation{
			Kind: input.LocationKind, Latitude: input.Latitude,
			Longitude: input.Longitude, Locality: input.Locality,
		}
		if !location.valid() {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		imageFile, err := input.Image.Open()
		if err != nil {
			return err
		}
		defer imageFile.Close()
		imageData, err := readImagePart(imageFile, input.Image)
		if err != nil {
			return err
		}
		if err := decodeUploadJPEG(c, imageData, 4096, "invalid_post"); err != nil {
			return err
		}

		ctx := c.Request().Context()
		// Persist immutable bytes and durable replication intent before taking DB locks.
		// Failed transactions may leave unreferenced files, which are never public.
		imageID := uuid.NewString()
		if err := h.ImageFiles.Save(ctx, imageID, imageData); err != nil {
			return err
		}
		return h.finishImageUpload(c, primaryImageUpload{Kind: "post", ImageID: imageID, Post: &input})
	}
}

func (h *HandlerContext) commitPost(c *echo.Context, input createPostRequest, imageID string) (createPostResponse, error) {
	ctx := c.Request().Context()
	userID := GetSession(c).UserID
	location := PostLocation{Kind: input.LocationKind, Latitude: input.Latitude, Longitude: input.Longitude, Locality: input.Locality}

	tx, err := h.WriteDB().BeginTxx(ctx, nil)
	if err != nil {
		return createPostResponse{}, err
	}
	defer tx.Rollback()
	var lockedUserID string
	if err := tx.GetContext(ctx, &lockedUserID, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID); err != nil {
		return createPostResponse{}, err
	}
	if err := recheckUploadSession(c, tx); err != nil {
		return createPostResponse{}, err
	}
	user, err := getUser(ctx, tx, userID)
	if err != nil {
		return createPostResponse{}, err
	}
	var window struct {
		ID             string    `db:"id"`
		LocalDate      time.Time `db:"local_date"`
		StartsAt       time.Time `db:"starts_at"`
		OnTimeDeadline time.Time `db:"on_time_deadline"`
	}
	// Concurrent authors may share this lock; ending the window must wait
	// for their commits. The user lock still serializes slot/count changes.
	err = tx.GetContext(ctx, &window, activePostingWindowQuery, user.Region)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.GetContext(ctx, &window, activePostingWindowQuery, user.Region)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return createPostResponse{}, NewAPIError(http.StatusConflict, "post_slot_unavailable")
		}
		return createPostResponse{}, err
	}
	var counts struct {
		Active  int `db:"active"`
		Deleted int `db:"deleted"`
	}
	if err := tx.GetContext(ctx, &counts, `
   SELECT count(*) FILTER (WHERE deleted_at IS NULL) AS active,
          count(*) FILTER (WHERE deleted_at IS NOT NULL) AS deleted
   FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
   WHERE p.author_id = $1 AND w.local_date = $2
  `, userID, window.LocalDate); err != nil {
		return createPostResponse{}, err
	}
	postedAt := time.Now().UTC()
	if counts.Active != 0 || counts.Deleted >= 2 || postedAt.Before(window.StartsAt) {
		return createPostResponse{}, NewAPIError(http.StatusConflict, "post_slot_unavailable")
	}
	// Match pgx timestamptz binary encoding (microsecond truncation) and
	// its default scan location, so the response matches a DB round trip.
	postedAt = postedAt.Truncate(time.Microsecond).In(time.Local)
	if _, err := tx.ExecContext(ctx, `INSERT INTO image_objects (id, owner_user_id, data, media_type, created_at) VALUES ($1, $2, $3, 'image/jpeg', $4)`, imageID, userID, nil, postedAt); err != nil {
		return createPostResponse{}, err
	}
	postID := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `
   INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility,
                      location_kind, latitude, longitude, locality, posted_at)
   VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
  `, postID, userID, window.ID, imageID, input.Caption, input.Visibility,
		location.Kind, location.Latitude, location.Longitude, location.Locality, postedAt); err != nil {
		return createPostResponse{}, err
	}
	post := Post{
		ID: postID, AuthorID: userID, PostingWindowID: window.ID,
		ImageID: imageID, Caption: input.Caption, Visibility: input.Visibility,
		PostLocation: location, PostedAt: postedAt, OnTimeDeadline: window.OnTimeDeadline,
	}
	if err := tx.Commit(); err != nil {
		return createPostResponse{}, err
	}
	return newPostResponse(post, user), nil
}

func newPostResponse(post Post, user User) createPostResponse {
	response := createPostResponse{
		ID: post.ID,
		Author: postAuthorResponse{
			ID: user.ID, Username: user.Username, DisplayName: user.DisplayName,
			ProfileImageURL: newGetMeResponse(user).ProfileImageURL,
			Bio:             user.Bio, AccountType: user.AccountType,
		},
		PostingWindowID: post.PostingWindowID, ImageURL: signedImageURL(user, post.ImageID),
		Caption: post.Caption, Visibility: post.Visibility, Location: post.PostLocation,
		PostedAt: post.PostedAt, OnTime: !post.PostedAt.After(post.OnTimeDeadline),
	}
	if !response.OnTime {
		response.LateSeconds = int64(post.PostedAt.Sub(post.OnTimeDeadline) / time.Second)
	}
	return response
}

type getMyCurrentPostResponse createPostResponse

func getMyCurrentPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		postID, err := h.currentPostID(c)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		var post Post
		if primaryScope(ctx) == nil {
			post, err = getPost(ctx, h.ReadDB(c), postID)
		} else {
			var payload PostWithAuthor
			payload, err = h.readPost(c, postID)
			post = payload.Post
		}
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		user, err := h.readUserPayload(c, userID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, getMyCurrentPostResponse(newPostResponse(post, user)))
	}
}

type getPostRequest struct {
	PostID string `param:"postId"`
}

type getPostResponse createPostResponse

func getPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getPostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		post, err := h.readPost(c, input.PostID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		viewer, err := loadViewerContext(ctx, h.ReadDB(c), GetSession(c).UserID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err != nil {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		posts, err := h.authorizePosts(c, viewer, []PostWithAuthor{post}, "")
		if err != nil {
			return err
		}
		if len(posts) == 0 || !viewer.canView(posts[0].Post) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		post = posts[0]
		return c.JSON(http.StatusOK, getPostResponse(newPostResponse(post.Post, post.Author)))
	}
}

type updatePostRequest struct {
	PostID     string  `param:"postId" json:"-"`
	Caption    *string `json:"caption"`
	Visibility *string `json:"visibility"`
}

type updatePostResponse createPostResponse

func updatePostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input updatePostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if input.Caption == nil && input.Visibility == nil {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if input.Caption != nil && !validUserText(*input.Caption, 0, 1000) {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if input.Visibility != nil {
			switch *input.Visibility {
			case "friends", "friends_of_friends", "public":
			default:
				return NewAPIError(http.StatusBadRequest, "invalid_post")
			}
		}
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		tx, err := h.WriteDB().BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var lockedUserID string
		if err := tx.GetContext(ctx, &lockedUserID, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID); err != nil {
			return err
		}
		post, err := getPost(ctx, tx, input.PostID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		if post.AuthorID != userID {
			return NewAPIError(http.StatusForbidden, "post_not_editable")
		}
		window, err := getPostingWindow(ctx, tx, post.PostingWindowID)
		if err != nil {
			return err
		}
		if window.EndedAt != nil {
			return NewAPIError(http.StatusForbidden, "post_not_editable")
		}
		if input.Caption != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE posts SET caption = $2 WHERE id = $1`, post.ID, *input.Caption); err != nil {
				return err
			}
		}
		if input.Visibility != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE posts SET visibility = $2 WHERE id = $1`, post.ID, *input.Visibility); err != nil {
				return err
			}
		}
		post, err = getPost(ctx, tx, post.ID)
		if err != nil {
			return err
		}
		user, err := getUser(ctx, tx, userID)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, updatePostResponse(newPostResponse(post, user)))
	}
}

type deletePostRequest struct {
	PostID string `param:"postId" json:"-"`
}

func deletePostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deletePostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		tx, err := h.WriteDB().BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var lockedUserID string
		if err := tx.GetContext(ctx, &lockedUserID, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID); err != nil {
			return err
		}
		post, err := getPost(ctx, tx, input.PostID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		if post.AuthorID != userID {
			return NewAPIError(http.StatusForbidden, "post_not_editable")
		}
		window, err := getPostingWindow(ctx, tx, post.PostingWindowID)
		if err != nil {
			return err
		}
		var deletions int
		if err := tx.GetContext(ctx, &deletions, `
   SELECT count(*) FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
   WHERE p.author_id = $1 AND w.local_date = $2 AND p.deleted_at IS NOT NULL
  `, userID, window.LocalDate); err != nil {
			return err
		}
		if deletions >= 2 {
			return NewAPIError(http.StatusConflict, "post_slot_unavailable")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE posts SET deleted_at = clock_timestamp() WHERE id = $1`, post.ID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

const activePostingWindowQuery = `SELECT id, local_date, starts_at, on_time_deadline FROM posting_windows WHERE region = $1 AND ended_at IS NULL FOR SHARE`
