package isureal

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type registerRequest struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Region      string `json:"region"`
}

type registerResponse struct {
	SecretKey string              `json:"secretKey"`
	User      sessionUserResponse `json:"user"`
}

type loginRequest struct {
	Username  string `json:"username"`
	SecretKey string `json:"secretKey"`
}

type loginResponse struct {
	Token string              `json:"token"`
	User  sessionUserResponse `json:"user"`
}

type sessionUserResponse struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Bio         string `json:"bio"`
	Region      string `json:"region"`
	AccountType string `json:"accountType"`
	Status      string `json:"status"`
}

type getMeResponse struct {
	sessionUserResponse
	ProfileImageURL *string `json:"profileImageUrl"`
}

type updateMeRequest struct {
	DisplayName *string `json:"displayName"`
	Bio         *string `json:"bio"`
	Region      *string `json:"region"`
}

type updateMeResponse getMeResponse

func newSessionUserResponse(user User) sessionUserResponse {
	return sessionUserResponse{
		ID: user.ID, Username: user.Username, DisplayName: user.DisplayName,
		Bio: user.Bio, Region: user.Region, AccountType: user.AccountType, Status: user.Status,
	}
}

func newGetMeResponse(user User) getMeResponse {
	response := getMeResponse{sessionUserResponse: newSessionUserResponse(user)}
	if user.ProfileImageID != nil {
		url := signedImageURL(user, *user.ProfileImageID)
		response.ProfileImageURL = &url
	}
	return response
}

func registerHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input registerRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		c.Set("username", input.Username)
		if !usernamePattern.MatchString(input.Username) || !validUserText(input.DisplayName, 1, 100) || !validRegion(input.Region) {
			return NewAPIError(http.StatusBadRequest, "invalid_registration")
		}
		secretKey := uuid.NewString()
		hash := sha256.Sum256([]byte(secretKey))
		ctx := c.Request().Context()
		tx, err := h.WriteDB().BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// INSERT takes ROW EXCLUSIVE until commit, which still conflicts with
		// the Posting Window opener's SHARE ROW EXCLUSIVE notification boundary.
		userID := uuid.NewString()
		if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, username, secret_key_hash, display_name, region) VALUES ($1, $2, $3, $4, $5)`,
			userID, input.Username, hex.EncodeToString(hash[:]), input.DisplayName, input.Region); err != nil {
			// Release the aborted transaction before consulting the catalog, even
			// when the connection pool has only one connection.
			tx.Rollback()
			if isUsernameConflict(ctx, h.WriteDB(), err) {
				return NewAPIError(http.StatusConflict, "username_taken")
			}
			return err
		}
		user, err := getUser(ctx, tx, userID)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, registerResponse{SecretKey: secretKey, User: newSessionUserResponse(user)})
	}
}

func loginHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input loginRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		c.Set("username", input.Username)
		if !usernamePattern.MatchString(input.Username) {
			return NewAPIError(http.StatusUnauthorized, "invalid_credentials")
		}
		hash := sha256.Sum256([]byte(input.SecretKey))
		ctx := c.Request().Context()
		tx, err := h.WriteDB().BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var user User
		err = tx.GetContext(ctx, &user, `SELECT id, username, secret_key_hash, display_name, bio, profile_image_id, region, account_type, status FROM users WHERE username = $1 AND secret_key_hash = $2 AND status = 'active' FOR NO KEY UPDATE`, input.Username, hex.EncodeToString(hash[:]))
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusUnauthorized, "invalid_credentials")
		}
		if err != nil {
			return err
		}
		session, err := createLoginSessionTx(ctx, tx, user.ID)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, loginResponse{Token: session.Token, User: newSessionUserResponse(user)})
	}
}

func logoutHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		session := GetSession(c)
		if session == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		if err := InvalidateLoginSession(h.WriteDB(), session.UserID, session.TokenHash); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

func getMeHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		session := GetSession(c)
		if session == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		user, err := h.readUserPayload(c, session.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if err != nil {
			return err
		}
		user.Region = session.Region
		return c.JSON(http.StatusOK, newGetMeResponse(user))
	}
}

func updateMeHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		session := GetSession(c)
		if session == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		var input updateMeRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.DisplayName == nil && input.Bio == nil && input.Region == nil) ||
			(input.DisplayName != nil && !validUserText(*input.DisplayName, 1, 100)) ||
			(input.Bio != nil && !validUserText(*input.Bio, 0, 500)) ||
			(input.Region != nil && !validRegion(*input.Region)) {
			return NewAPIError(http.StatusBadRequest, "invalid_profile")
		}
		ctx := c.Request().Context()
		tx, err := h.WriteDB().BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if input.DisplayName != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET display_name = $2, updated_at = clock_timestamp() WHERE id = $1`, session.UserID, *input.DisplayName); err != nil {
				return err
			}
		}
		if input.Bio != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET bio = $2, updated_at = clock_timestamp() WHERE id = $1`, session.UserID, *input.Bio); err != nil {
				return err
			}
		}
		if input.Region != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET region = $2, updated_at = clock_timestamp() WHERE id = $1`, session.UserID, *input.Region); err != nil {
				return err
			}
		}
		user, err := getUser(ctx, tx, session.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, updateMeResponse(newGetMeResponse(user)))
	}
}

// PostgreSQL reports the unique index name even for standalone UNIQUE indexes.
// Inspect its key columns rather than assuming the externally managed index name
// or interpreting localized error detail text. Other unique violations propagate.
func isUsernameConflict(ctx context.Context, db sqlx.QueryerContext, err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.TableName != "users" {
		return false
	}
	var matches bool
	catalogErr := sqlx.GetContext(ctx, db, &matches, `SELECT EXISTS (
        SELECT 1 FROM pg_index i
        JOIN pg_class t ON t.oid=i.indrelid
        JOIN pg_namespace n ON n.oid=t.relnamespace
        JOIN pg_class idx ON idx.oid=i.indexrelid
        JOIN pg_attribute a ON a.attrelid=t.oid AND a.attnum=i.indkey[0]
        WHERE n.nspname=$1 AND t.relname='users' AND idx.relname=$2
          AND i.indisunique AND i.indnkeyatts=1 AND a.attname='username'
          AND i.indexprs IS NULL AND i.indpred IS NULL
    )`, pgErr.SchemaName, pgErr.ConstraintName)
	return catalogErr == nil && matches
}
