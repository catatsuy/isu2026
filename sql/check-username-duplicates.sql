-- 全statusを対象に確認する。0行なら、この時点でusernameの重複はない。
SELECT username, count(*) AS duplicate_count, array_agg(id ORDER BY id) AS user_ids
FROM users
GROUP BY username
HAVING count(*) > 1
ORDER BY username;
