-- psql -X -v ON_ERROR_STOP=1 -f sql/add-query-indexes.sql
-- BEGIN / psql --single-transaction (-1) 内では実行しない。
\set ON_ERROR_STOP on
\set AUTOCOMMIT on

-- 重複を表示して停止する。データは変更しない。
\ir check-username-duplicates.sql
SELECT EXISTS (SELECT 1 FROM users GROUP BY username HAVING count(*) > 1) AS has_duplicates \gset
\if :has_duplicates
  \echo 'ERROR: duplicate usernames found; resolve explicitly before retrying.'
  \quit 1
\endif

-- 事前確認後に発生した重複もUNIQUEインデックス作成時に検出される。
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS users_username_unique ON users(username);
CREATE INDEX CONCURRENTLY IF NOT EXISTS users_active_username_id ON users(username, id)
  WHERE status = 'active';
CREATE INDEX CONCURRENTLY IF NOT EXISTS posts_active_image_author ON posts(image_id, author_id)
  WHERE deleted_at IS NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS realmojis_active_image_reactor ON realmojis(image_id, reactor_id)
  INCLUDE (post_id) WHERE replaced_at IS NULL AND deleted_at IS NULL;
-- 正順は既存UNIQUE(user_low_id,user_high_id)を利用する。
CREATE INDEX CONCURRENTLY IF NOT EXISTS friendships_high_low ON friendships(user_high_id, user_low_id);
