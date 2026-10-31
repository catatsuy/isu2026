-- psql変数はREADME-indexes.mdを参照。実データに存在する値を渡す。
\set ON_ERROR_STOP on

EXPLAIN (ANALYZE, BUFFERS)
SELECT i.media_type, u.secret_key_hash,
   CASE
     WHEN u.profile_image_id = i.id THEN true
     WHEN EXISTS (
       SELECT 1 FROM posts p
       WHERE p.author_id = u.id AND p.image_id = i.id AND p.deleted_at IS NULL
     ) THEN true
     ELSE EXISTS (
       SELECT 1 FROM realmojis r
       JOIN posts p ON p.id = r.post_id AND p.deleted_at IS NULL
       JOIN users author ON author.id = p.author_id AND author.status = 'active'
       WHERE r.reactor_id = u.id AND r.image_id = i.id
         AND r.replaced_at IS NULL AND r.deleted_at IS NULL
     )
   END AS visible
 FROM image_objects i
 JOIN users u ON u.id = i.owner_user_id AND u.status = 'active'
 WHERE i.id = :'image_id';

-- go/feed_api.go / go/post.go: 代表例は2ユーザー。実際のINリスト件数でも比較する。
EXPLAIN (ANALYZE, BUFFERS)
SELECT user_low_id, user_high_id FROM friendships
WHERE user_low_id IN (:'friend_id_1', :'friend_id_2')
   OR user_high_id IN (:'friend_id_1', :'friend_id_2');

-- go/user_read_api.go: 初回はbefore_usernameとbefore_idを空文字にする。
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, username, secret_key_hash, display_name, bio, profile_image_id, region, account_type, status
FROM users
WHERE status='active' AND id<>:'viewer_id'
  AND (:'before_username'='' OR (username,id)>(:'before_username',:'before_id'))
ORDER BY username, id;

-- go/user_api.go: 登録時のusername存在確認。
EXPLAIN (ANALYZE, BUFFERS)
SELECT EXISTS(SELECT 1 FROM users WHERE username = :'username');
