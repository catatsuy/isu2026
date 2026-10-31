package isureal

import (
	"testing"
)

// Exercise the actual SQL against isolated temporary tables, without touching
// application tables. This test is skipped unless a local test DB is supplied.
func TestImageAccessSQLPostgres(t *testing.T) {
	db := localPostgresTestDB(t)
	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`CREATE TEMP TABLE users (id text PRIMARY KEY, status text, profile_image_id text, secret_key_hash text) ON COMMIT DROP`,
		`CREATE TEMP TABLE image_objects (id text PRIMARY KEY, owner_user_id text, media_type text) ON COMMIT DROP`,
		`CREATE TEMP TABLE posts (id text PRIMARY KEY, author_id text, image_id text, deleted_at timestamptz) ON COMMIT DROP`,
		`CREATE TEMP TABLE realmojis (post_id text, reactor_id text, image_id text, replaced_at timestamptz, deleted_at timestamptz) ON COMMIT DROP`,
		`INSERT INTO users VALUES ('owner','active',NULL,'key'), ('author','active',NULL,'author-key')`,
		`INSERT INTO image_objects VALUES ('image','owner','image/jpeg')`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, change string
		visible      bool
	}{
		{"unreferenced", `SELECT 1`, false},
		{"profile", `UPDATE users SET profile_image_id='image' WHERE id='owner'`, true},
		{"removed profile", `UPDATE users SET profile_image_id=NULL`, false},
		{"post", `INSERT INTO posts VALUES ('post','owner','image',NULL)`, true},
		{"deleted post", `UPDATE posts SET deleted_at=now()`, false},
		{"wrong post owner", `UPDATE posts SET deleted_at=NULL, author_id='author'`, false},
		{"reaction", `INSERT INTO realmojis VALUES ('post','owner','image',NULL,NULL)`, true},
		{"replaced reaction", `UPDATE realmojis SET replaced_at=now()`, false},
		{"deleted reaction", `UPDATE realmojis SET replaced_at=NULL, deleted_at=now()`, false},
		{"wrong reactor", `UPDATE realmojis SET deleted_at=NULL, reactor_id='author'`, false},
		{"restored reaction", `UPDATE realmojis SET reactor_id='owner'`, true},
		{"deleted target", `UPDATE posts SET deleted_at=now()`, false},
		{"restored target", `UPDATE posts SET deleted_at=NULL`, true},
		{"inactive target author", `UPDATE users SET status='deleted' WHERE id='author'`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tx.Exec(tc.change); err != nil {
				t.Fatal(err)
			}
			var object struct {
				MediaType     string `db:"media_type"`
				SecretKeyHash string `db:"secret_key_hash"`
				Visible       bool   `db:"visible"`
			}
			if err := tx.Get(&object, imageAccessQuery, "image"); err != nil {
				t.Fatal(err)
			}
			if object.Visible != tc.visible || object.MediaType != "image/jpeg" || object.SecretKeyHash != "key" {
				t.Fatalf("unexpected authorization: %+v", object)
			}
		})
	}
	if _, err := tx.Exec(`UPDATE users SET status='deleted' WHERE id='owner'`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(imageAccessQuery, "image")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("inactive image owner was authorized")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
