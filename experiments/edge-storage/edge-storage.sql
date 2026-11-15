-- MySQL 8.0, run once in a fresh, empty database.
CREATE TEMPORARY TABLE digits (n INT PRIMARY KEY);
INSERT INTO digits VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9);
CREATE TABLE like_base (
  id BIGINT UNSIGNED PRIMARY KEY AUTO_INCREMENT,
  account_id BIGINT UNSIGNED NOT NULL,
  video_id BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(3) NOT NULL,
  UNIQUE KEY pair_key (video_id, account_id),
  KEY recent (account_id, created_at, video_id)
) ENGINE=InnoDB;
CREATE TABLE like_pair (
  account_id BIGINT UNSIGNED NOT NULL,
  video_id BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (account_id, video_id),
  KEY recent (account_id, created_at, video_id)
) ENGINE=InnoDB;
CREATE TABLE follow_base (
  id BIGINT UNSIGNED PRIMARY KEY AUTO_INCREMENT,
  follower_id BIGINT UNSIGNED NOT NULL,
  vlogger_id BIGINT UNSIGNED NOT NULL,
  UNIQUE KEY pair_key (follower_id, vlogger_id),
  KEY outgoing (follower_id),
  KEY incoming (vlogger_id)
) ENGINE=InnoDB;
CREATE TABLE follow_pair (
  follower_id BIGINT UNSIGNED NOT NULL,
  vlogger_id BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (follower_id, vlogger_id),
  KEY incoming (vlogger_id, follower_id)
) ENGINE=InnoDB;

-- Six digits produce one million distinct sequence numbers.
CREATE TABLE digit_source AS SELECT n FROM digits;
INSERT INTO like_base (account_id, video_id, created_at)
SELECT MOD(seq,10000)+1, MOD(MOD(seq,10000)*97+FLOOR(seq/10000)*7919,100003)+1,
       TIMESTAMPADD(SECOND,seq,'2025-01-01')
FROM (SELECT a.n+10*b.n+100*c.n+1000*d.n+10000*e.n+100000*f.n AS seq
      FROM digit_source a CROSS JOIN digit_source b CROSS JOIN digit_source c
      CROSS JOIN digit_source d CROSS JOIN digit_source e CROSS JOIN digit_source f) s
ORDER BY seq;
INSERT INTO like_pair SELECT account_id,video_id,created_at FROM like_base ORDER BY id;
INSERT INTO follow_base (follower_id,vlogger_id) SELECT account_id,video_id+10000 FROM like_base ORDER BY id;
INSERT INTO follow_pair SELECT follower_id,vlogger_id FROM follow_base ORDER BY id;
ANALYZE TABLE like_base,like_pair,follow_base,follow_pair;
SET SESSION information_schema_stats_expiry=0;
SELECT VERSION(), @@innodb_page_size;
SELECT table_name, data_length, index_length, (data_length+index_length)/1000000 AS bytes_per_edge
FROM information_schema.tables
WHERE table_schema=DATABASE() AND table_name IN ('like_base','like_pair','follow_base','follow_pair')
ORDER BY table_name;
SELECT (SELECT COUNT(*) FROM like_base) AS like_base_rows,
       (SELECT COUNT(*) FROM like_pair) AS like_pair_rows,
       (SELECT COUNT(*) FROM follow_base) AS follow_base_rows,
       (SELECT COUNT(*) FROM follow_pair) AS follow_pair_rows;
CREATE TABLE like_current LIKE like_base;
ALTER TABLE like_current DROP INDEX recent;
INSERT INTO like_current SELECT * FROM like_base ORDER BY id;
ANALYZE TABLE like_current;
SET SESSION information_schema_stats_expiry=0;
SELECT data_length, index_length,(data_length+index_length)/1000000 AS bytes_per_edge
FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='like_current';
EXPLAIN SELECT video_id FROM like_pair WHERE account_id=123 AND video_id IN (11835,11912,12671);
EXPLAIN SELECT video_id FROM like_pair WHERE account_id=123 ORDER BY created_at DESC,video_id DESC LIMIT 20;
EXPLAIN SELECT follower_id FROM follow_pair WHERE vlogger_id=22000 AND follower_id>300 LIMIT 20;
-- Each direction can now be routed independently to its owner's shard.
CREATE TABLE follow_out (
  follower_id BIGINT UNSIGNED NOT NULL,
  vlogger_id BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (follower_id, vlogger_id)
) ENGINE=InnoDB;
CREATE TABLE follow_in (
  vlogger_id BIGINT UNSIGNED NOT NULL,
  follower_id BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (vlogger_id, follower_id)
) ENGINE=InnoDB;
INSERT INTO follow_out SELECT follower_id,vlogger_id FROM follow_base ORDER BY id;
INSERT INTO follow_in SELECT vlogger_id,follower_id FROM follow_base ORDER BY id;
ANALYZE TABLE follow_out,follow_in;
SELECT SUM(data_length+index_length)/1000000 AS both_directions_bytes_per_edge
FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ('follow_out','follow_in');
