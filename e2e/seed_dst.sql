-- Destination-side type-trap tables: same data with tolerated or
-- intentionally-different shapes.
SET time_zone = '+00:00';

-- -04:00 local 00:00 == UTC 04:00: same instant as src's +08:00 12:00
DROP TABLE IF EXISTS tz_ts;
CREATE TABLE tz_ts (id INT PRIMARY KEY, ts TIMESTAMP);
SET time_zone = '-04:00';
INSERT INTO tz_ts VALUES (1, '2024-06-01 00:00:00');

-- different naive wall clock -> genuinely different
DROP TABLE IF EXISTS tz_dt;
CREATE TABLE tz_dt (id INT PRIMARY KEY, dt DATETIME);
INSERT INTO tz_dt VALUES (1, '2024-06-01 00:00:00');

-- -04:00 local 00:00 == UTC 04:00, matching src's naive wall clock;
-- comparable only with --allow-tz-swap. Column name matches src ("dt").
DROP TABLE IF EXISTS dt_ts_swap;
CREATE TABLE dt_ts_swap (id INT PRIMARY KEY, dt TIMESTAMP);
SET time_zone = '-04:00';
INSERT INTO dt_ts_swap VALUES (1, '2024-06-01 00:00:00');

-- within 1e-9 tolerance
DROP TABLE IF EXISTS t_float_small;
CREATE TABLE t_float_small (id INT PRIMARY KEY, f DOUBLE);
INSERT INTO t_float_small VALUES (1, 1.0 + 1e-12);

-- 0.01 apart: differs under any sane tolerance
DROP TABLE IF EXISTS t_float_big;
CREATE TABLE t_float_big (id INT PRIMARY KEY, f DOUBLE);
INSERT INTO t_float_big VALUES (1, 1.01);

-- same JSON semantics, different raw text (2.0 vs 2)
DROP TABLE IF EXISTS t_json;
CREATE TABLE t_json (id INT PRIMARY KEY, j JSON);
INSERT INTO t_json VALUES (1, '{"b": 2.0, "a": 1}');

-- different decimal width; values normalize to the same
DROP TABLE IF EXISTS t_dec;
CREATE TABLE t_dec (id INT PRIMARY KEY, dec_val DECIMAL(12,3));
INSERT INTO t_dec VALUES (1, 1.000), (2, 0), (3, 0.1);

-- CHAR vs VARCHAR, value without padding: equal after trim
DROP TABLE IF EXISTS t_char;
CREATE TABLE t_char (id INT PRIMARY KEY, ch VARCHAR(10));
INSERT INTO t_char VALUES (1, 'ab');

-- bit(1) vs bit(8): same numeric values
DROP TABLE IF EXISTS t_bit;
CREATE TABLE t_bit (id INT PRIMARY KEY, b BIT(8));
INSERT INTO t_bit VALUES (1, 0x01), (2, 0x00);

-- identical NULL/empty layout
DROP TABLE IF EXISTS t_null;
CREATE TABLE t_null (id INT PRIMARY KEY, a VARCHAR(10), b VARCHAR(10));
INSERT INTO t_null VALUES (1, NULL, ''), (2, '', NULL);

-- NULL vs empty string: must differ
DROP TABLE IF EXISTS t_nulltrap;
CREATE TABLE t_nulltrap (id INT PRIMARY KEY, a VARCHAR(10));
INSERT INTO t_nulltrap VALUES (1, '');

DROP TABLE IF EXISTS t_enum;
CREATE TABLE t_enum (id INT PRIMARY KEY, e ENUM('a','b'));
INSERT INTO t_enum VALUES (1, 'a'), (2, 'b');

DROP TABLE IF EXISTS t_enumtrap;
CREATE TABLE t_enumtrap (id INT PRIMARY KEY, e ENUM('a','b'));
INSERT INTO t_enumtrap VALUES (1, 'b');

-- Round-8 seeds (dst variants).

-- identical TIME data
DROP TABLE IF EXISTS t_time;
CREATE TABLE t_time (id INT PRIMARY KEY, v TIME(6));
INSERT INTO t_time VALUES
  (1, '00:00:00'), (2, '01:02:03'), (3, '-01:02:03'), (4, '838:59:59'),
  (5, '-838:59:59'), (6, '00:00:00.1'), (7, '00:00:00.01'),
  (8, '00:00:00.000001');

-- BIGINT UNSIGNED where the src declares INT (cross-family numeric)
DROP TABLE IF EXISTS t_numfam;
CREATE TABLE t_numfam (id INT PRIMARY KEY, v BIGINT UNSIGNED);
INSERT INTO t_numfam VALUES (1, 1);

-- row 1 diverges: 2e10 vs 1e10 (within no sane tolerance of each other)
DROP TABLE IF EXISTS t_float_big2;
CREATE TABLE t_float_big2 (id INT PRIMARY KEY, f DOUBLE);
INSERT INTO t_float_big2 VALUES (1, 2e10), (2, 1e12);

-- 9007199254740993: beyond 2^53, stored exactly by MySQL
DROP TABLE IF EXISTS t_jsonbig;
CREATE TABLE t_jsonbig (id INT PRIMARY KEY, j JSON);
INSERT INTO t_jsonbig VALUES (1, '{"n":9007199254740993}');
-- the double 1.0 where the src holds the integer 1: equal only after
-- normalization
DROP TABLE IF EXISTS t_jsonbig_ok;
CREATE TABLE t_jsonbig_ok (id INT PRIMARY KEY, j JSON);
INSERT INTO t_jsonbig_ok VALUES (1, '{"a":1.0}');

-- row 1's b payload differs in length and content; row 2 (1MiB) identical
DROP TABLE IF EXISTS t_bigblob;
CREATE TABLE t_bigblob (id INT PRIMARY KEY, a LONGBLOB, b LONGBLOB);
INSERT INTO t_bigblob VALUES
  (1, REPEAT(0x41, 65536), REPEAT(0x42, 65537)),
  (2, REPEAT(0x43, 1048576), REPEAT(0x44, 1048576));
DROP TABLE IF EXISTS t_bigblob_ok;
CREATE TABLE t_bigblob_ok (id INT PRIMARY KEY, a LONGBLOB);
INSERT INTO t_bigblob_ok VALUES (1, REPEAT(0x45, 1048576));

-- the default (case-insensitive) collation where the src is utf8mb4_bin;
-- identical data, so the only difference is the key's ordering semantics
DROP TABLE IF EXISTS t_keycoll;
CREATE TABLE t_keycoll (k VARCHAR(16) COLLATE utf8mb4_general_ci PRIMARY KEY, v INT);
INSERT INTO t_keycoll VALUES ('Z', 1), ('a', 2);

-- the parent's data DIFFERS (the 'a' row); the child is identical
DROP TABLE IF EXISTS t_keyfk_child;
DROP TABLE IF EXISTS t_keyfk;
CREATE TABLE t_keyfk (k VARCHAR(16) COLLATE utf8mb4_general_ci PRIMARY KEY, v INT);
INSERT INTO t_keyfk VALUES ('Z', 1), ('a', 99);
CREATE TABLE t_keyfk_child (k VARCHAR(16) COLLATE utf8mb4_general_ci NOT NULL,
  c INT PRIMARY KEY,
  CONSTRAINT fk_kfc FOREIGN KEY (k) REFERENCES t_keyfk (k) ON DELETE CASCADE);
INSERT INTO t_keyfk_child VALUES ('Z', 100), ('a', 200);

-- the literal 08:00 under +08:00 is the 00:00 UTC instant: a DIFFERENT
-- instant from the src's 08:00 UTC, displaying identically under the two
-- servers' default zones
DROP TABLE IF EXISTS t_timestamp_tz;
CREATE TABLE t_timestamp_tz (id INT PRIMARY KEY, ts TIMESTAMP);
SET time_zone = '+08:00';
INSERT INTO t_timestamp_tz VALUES (1, '2024-01-01 08:00:00');
SET time_zone = '+00:00';

-- round-9: the JSON STRING where the src holds the JSON number (P0-1):
-- {"n":1} and {"n":"1"} must stay DIFFERENT under --normalize-json
DROP TABLE IF EXISTS t_json_type;
CREATE TABLE t_json_type (id INT PRIMARY KEY, j JSON);
INSERT INTO t_json_type VALUES (1, '{"n":"1"}');
-- number 1.0 where the src holds number 1
DROP TABLE IF EXISTS t_json_type_ok;
CREATE TABLE t_json_type_ok (id INT PRIMARY KEY, j JSON);
INSERT INTO t_json_type_ok VALUES (1, '{"n":1.0}');

-- round-9: the ENUM members DEFINED in the opposite order (P0-2)
DROP TABLE IF EXISTS t_enumkey;
CREATE TABLE t_enumkey (k ENUM('a','b') PRIMARY KEY, v INT);
INSERT INTO t_enumkey VALUES ('a', 1), ('b', 2);
-- identical schema drift, and the 'b' row's value DIFFERS
DROP TABLE IF EXISTS t_enumkey_drift;
CREATE TABLE t_enumkey_drift (k ENUM('a','b') PRIMARY KEY, v INT);
INSERT INTO t_enumkey_drift VALUES ('a', 1), ('b', 99);

-- round-9: DOUBLE where the src holds exact BIGINT / DECIMAL (P1-3)
DROP TABLE IF EXISTS t_numfam_large;
CREATE TABLE t_numfam_large (id INT PRIMARY KEY, v DOUBLE);
INSERT INTO t_numfam_large VALUES (1, 1000000);
DROP TABLE IF EXISTS t_numfam_dec;
CREATE TABLE t_numfam_dec (id INT PRIMARY KEY, v DOUBLE);
INSERT INTO t_numfam_dec VALUES (1, 0.00001);
