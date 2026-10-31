package isureal

import (
	"bytes"
	"image/jpeg"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// Only the signed Primary RPC accepts this payload. External handlers always
// decode a JPEG, generate a fresh UUID and durably Save before constructing it.
type primaryImageUpload struct {
	Kind         string             `json:"kind"`
	ImageID      string             `json:"image_id"`
	Post         *createPostRequest `json:"post,omitempty"`
	PostID       string             `json:"post_id,omitempty"`
	ReactionType string             `json:"reaction_type,omitempty"`
}
type primaryImageUploadResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
}

func localImageUpload(c *echo.Context) bool {
	switch c.Path() {
	case "/api/posts", "/api/posts/:postId/realmojis":
		return c.Request().Method == http.MethodPost
	case "/api/me/profile-image":
		return c.Request().Method == http.MethodPut
	}
	return false
}

func (u primaryImageUpload) validate() error {
	id, err := uuid.Parse(u.ImageID)
	if err != nil || id.String() != u.ImageID {
		return NewAPIError(400, "invalid_primary_request")
	}
	switch u.Kind {
	case "post":
		if u.Post == nil || !validUserText(u.Post.Caption, 0, 1000) {
			return NewAPIError(400, "invalid_post")
		}
		switch u.Post.Visibility {
		case "friends", "friends_of_friends", "public":
		default:
			return NewAPIError(400, "invalid_post")
		}
		location := PostLocation{Kind: u.Post.LocationKind, Latitude: u.Post.Latitude, Longitude: u.Post.Longitude, Locality: u.Post.Locality}
		if !location.valid() {
			return NewAPIError(400, "invalid_post")
		}
	case "profile":
	case "realmoji":
		if u.PostID == "" {
			return NewAPIError(400, "invalid_realmoji")
		}
		switch u.ReactionType {
		case "like", "smile", "surprise", "heart", "laugh", "instant":
		default:
			return NewAPIError(400, "invalid_realmoji")
		}
	default:
		return NewAPIError(400, "invalid_primary_request")
	}
	return nil
}

func (h *HandlerContext) commitImageUpload(c *echo.Context, u primaryImageUpload) (primaryImageUploadResult, error) {
	started := time.Now()
	defer func() {
		log.Printf("image_upload phase=db kind=%s image_id=%s duration_us=%d", u.Kind, u.ImageID, time.Since(started).Microseconds())
	}()
	result := primaryImageUploadResult{Status: http.StatusCreated}
	var value any
	var err error
	switch u.Kind {
	case "post":
		value, err = h.commitPost(c, *u.Post, u.ImageID)
	case "profile":
		result.Status = http.StatusNoContent
		err = h.commitProfileImage(c, u.ImageID)
	case "realmoji":
		value, err = h.commitRealMoji(c, replaceRealMojiRequest{PostID: u.PostID, ReactionType: u.ReactionType}, u.ImageID)
	}
	if err != nil {
		return result, err
	}
	if result.Status != http.StatusNoContent {
		result.Body, err = json.Marshal(value)
	}
	return result, err
}

func (h *HandlerContext) finishImageUpload(c *echo.Context, u primaryImageUpload) error {
	started := time.Now()
	defer func() {
		log.Printf("image_upload phase=metadata kind=%s image_id=%s duration_us=%d", u.Kind, u.ImageID, time.Since(started).Microseconds())
	}()
	var result primaryImageUploadResult
	var err error
	if h.PrimaryAPI != nil {
		err = h.callPrimary(c, primaryRPCRequest{Op: "image_upload", Upload: &u}, &result)
	} else {
		result, err = h.commitImageUpload(c, u)
	}
	if err != nil {
		return err
	}
	if result.Status == http.StatusNoContent {
		return c.NoContent(result.Status)
	}
	if result.Status != http.StatusCreated || !json.Valid(result.Body) {
		return NewAPIError(502, "primary_api_invalid_response")
	}
	// Echo's JSON encoder terminates responses with a newline.
	return c.Blob(result.Status, "application/json", append(result.Body, '\n'))
}

// Call only after holding the actor's user-row lock. Logout, session replacement,
// region changes and account deletion serialize on that same lock. Recheck the
// original credentials here, rather than trusting the pre-upload authorization.
func recheckUploadSession(c *echo.Context, tx *sqlx.Tx) error {
	session := GetSession(c)
	if session.TokenHash == "" {
		return nil
	} // Callers with an injected session have no HTTP credentials.
	var valid bool
	err := tx.GetContext(c.Request().Context(), &valid, `SELECT EXISTS(
 SELECT FROM login_sessions s JOIN users u ON u.id=s.user_id
 WHERE s.user_id=$1 AND s.token_hash=$2 AND s.active AND u.status='active' AND u.region=$3)`, session.UserID, session.TokenHash, session.Region)
	if err != nil {
		return err
	}
	if !valid {
		return NewAPIError(http.StatusUnauthorized, "invalid_session")
	}
	// An IP can become banned while the image is being saved.
	var banned bool
	err = tx.GetContext(c.Request().Context(), &banned, `SELECT EXISTS(SELECT FROM ip_bans WHERE ip=$1 AND banned_until>clock_timestamp())`, c.RealIP())
	if err != nil {
		return err
	}
	if banned {
		return NewAPIError(http.StatusForbidden, "ip_banned")
	}
	return nil
}

func trustedUploadRPC(c *echo.Context) bool {
	region, _ := c.Get("primary_forward_region").(string)
	nonce, _ := c.Get("primary_request_nonce").(string)
	return (region == "europe" || region == "america") && nonce != "" && strings.HasPrefix(c.Request().Header.Get("Authorization"), "Bearer ")
}

func decodeUploadJPEG(c *echo.Context, data []byte, limit int, code string) error {
	started := time.Now()
	img, err := jpeg.Decode(bytes.NewReader(data))
	log.Printf("image_upload phase=jpeg route=%s bytes=%d duration_us=%d", c.Path(), len(data), time.Since(started).Microseconds())
	if err != nil {
		if code == "invalid_profile_image" {
			c.Set("abuse", "invalid_image")
		}
		return NewAPIError(400, code)
	}
	if img.Bounds().Dx() > limit || img.Bounds().Dy() > limit {
		return NewAPIError(400, code)
	}
	return nil
}
