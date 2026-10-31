package isureal

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type HandlerContext struct {
	// DB is the legacy Primary alias used by write handlers and fixtures.
	DB            *sqlx.DB
	PrimaryDB     *sqlx.DB
	ReplicaDB     *sqlx.DB
	Replication   *replicaRouter
	PrimaryAPI    *primaryAPIClient
	Config        *Config
	IPBans        *IPBanList
	ImageFiles    *ImageFileStore
	Notifications *notificationHub
}

func NewServer(config *Config) *echo.Echo {
	server, _ := NewServerWithCleanup(config)
	return server
}

// NewServerWithCleanup returns a cleanup function to call after HTTP shutdown.
func NewServerWithCleanup(config *Config) (*echo.Echo, func()) {
	if !validRegion(config.Region) {
		panic("invalid server region: " + config.Region)
	}
	if config.Region != "asia" && config.PrimaryAPIURL == "" {
		panic("ISUREAL_PRIMARY_API_URL is required outside asia")
	}
	var primaryAPI *primaryAPIClient
	if config.PrimaryAPIURL != "" {
		token, err := os.ReadFile(config.ImageTokenFile)
		if err != nil {
			panic(err)
		}
		primaryAPI, err = newPrimaryAPIClient(config.PrimaryAPIURL, config.Region, token)
		if err != nil {
			panic(err)
		}
	}
	e := echo.New()
	e.JSONSerializer = goJSONSerializer{}
	e.IPExtractor = extractClientIP

	e.Use(ActionLogMiddleware(config.LogLevel, os.Stdout))
	e.Use(middleware.Recover())
	e.HTTPErrorHandler = APIErrorHandler

	var db *sqlx.DB
	if primaryAPI == nil {
		var err error
		db, err = sqlx.Open("pgx", config.DatabaseDSN())
		if err != nil {
			panic("failed to open database: " + err.Error())
		}
		maxConns := config.DBMaxOpenConns
		if maxConns == 0 {
			maxConns = 25
		}
		if maxConns < 0 {
			panic("DBMaxOpenConns must be a positive integer")
		}
		db.SetMaxOpenConns(maxConns)
		db.SetMaxIdleConns(maxConns)
		db.SetConnMaxLifetime(3 * time.Minute)
		db.SetConnMaxIdleTime(2 * time.Minute)
	}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var err error
		if primaryAPI != nil {
			err = primaryAPI.call(ctx, "", "", primaryRPCRequest{Op: "ready"}, nil)
		} else {
			err = db.PingContext(ctx)
		}
		cancel()
		if err == nil {
			break
		}
		log.Printf("waiting for database: %v", err)
		time.Sleep(2 * time.Second)
	}
	log.Print("DB ready!")

	imageFiles, err := NewImageFileStore(config)
	if err != nil {
		panic(err)
	}
	handlerContext := &HandlerContext{
		DB:            db,
		PrimaryAPI:    primaryAPI,
		Config:        config,
		IPBans:        &IPBanList{},
		ImageFiles:    imageFiles,
		Notifications: newNotificationHub(db),
	}
	handlerContext.PrimaryDB = db
	e.Use(AsiaRoutingMiddleware(handlerContext))
	if config.Region != "asia" {
		e.Use(WriteDeadlineMiddleware)
	}
	e.Use(ReadRoutingMiddleware(handlerContext))
	e.Use(IPBanMiddleware(handlerContext))

	e.PUT("/internal/image-files/:imageId", imageFiles.peerHandler())
	e.GET("/internal/image-files/:imageId", imageFiles.peerHandler())
	e.POST(primaryRPCPath, primaryRPCHandler(handlerContext))
	e.POST("/api/initialize", initializeHandler(handlerContext))
	e.POST("/api/auth/register", registerHandler(handlerContext))
	e.POST("/api/auth/login", loginHandler(handlerContext))
	e.POST("/api/auth/logout", logoutHandler(handlerContext), SessionMiddleware(handlerContext, true))
	e.GET("/api/users", searchUsersHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.GET("/api/users/:userId", getUserHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.GET("/api/me/export", exportPostsHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.GET("/api/me", getMeHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.PATCH("/api/me", updateMeHandler(handlerContext), SessionMiddleware(handlerContext, true))
	e.PUT("/api/me/profile-image", putProfileImageHandler(handlerContext), SessionMiddleware(handlerContext, true))
	e.DELETE("/api/me/profile-image", deleteProfileImageHandler(handlerContext), SessionMiddleware(handlerContext, true))
	e.POST("/api/me/account-deletion", deleteAccountHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.POST("/api/posts", createPostHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.GET("/api/posting-window/current", getCurrentPostingWindowHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.GET("/api/posts/me/current", getMyCurrentPostHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.GET("/api/posts/:postId", getPostHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.PATCH("/api/posts/:postId", updatePostHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.DELETE("/api/posts/:postId", deletePostHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.GET("/api/feeds/:feed", getFeedHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.GET("/api/memories", getMemoriesHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.GET("/api/memories/:date", getMemoriesByDateHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.POST("/api/posts/:postId/comments", createCommentHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.GET("/api/posts/:postId/comments", getCommentsHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.DELETE("/api/comments/:commentId", deleteCommentHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.POST("/api/posts/:postId/realmojis", replaceRealMojiHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.GET("/api/posts/:postId/realmojis", getRealMojisHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.DELETE("/api/realmojis/:realMojiId", deleteRealMojiHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.GET("/api/images/:imageId", getImageHandler(handlerContext))
	e.GET("/api/notifications", getNotificationsHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.POST("/api/friend-requests", createFriendRequestHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.POST("/api/friend-requests/:requestId/:action", transitionFriendRequestHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.GET("/api/relationships/:kind", getRelationshipsHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.DELETE("/api/friendships/:userId", deleteFriendshipHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.PUT("/api/follows/:userId", followHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.DELETE("/api/follows/:userId", unfollowHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.PUT("/api/blocks/:userId", blockHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.DELETE("/api/blocks/:userId", unblockHandler(handlerContext), SessionMiddleware(handlerContext, true), RegionMiddleware(handlerContext))
	e.POST("/api/admin/auth/login", adminLoginHandler(handlerContext))
	e.POST("/api/admin/auth/logout", adminLogoutHandler(handlerContext), AdminSessionMiddleware(handlerContext, true))
	e.PUT("/api/admin/users/:userId/certification", certifyUserHandler(handlerContext), AdminSessionMiddleware(handlerContext, true))
	e.DELETE("/api/admin/users/:userId/certification", deleteUserCertificationHandler(handlerContext), AdminSessionMiddleware(handlerContext, true))
	e.POST("/api/admin/posting-window", openPostingWindowHandler(handlerContext), AdminSessionMiddleware(handlerContext, true))

	return e, func() {
		if primaryAPI != nil {
			primaryAPI.client.CloseIdleConnections()
		}
		imageFiles.Close()
		if db != nil {
			db.Close()
		}
	}
}
