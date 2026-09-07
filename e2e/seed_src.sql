-- Source-side type-trap tables.
SET time_zone = '+00:00';

-- TIMESTAMP stores the UTC instant; the literal is interpreted in the
-- session time zone. +08:00 local 12:00 == UTC 04:00.
DROP TABLE IF EXISTS tz_ts;
CREATE TABLE tz_ts (id INT PRIMARY KEY, ts TIMESTAMP);
SET time_zone = '+08:00';
INSERT INTO tz_ts VALUES (1, '2024-06-01 12:00:00');

DROP TABLE IF EXISTS tz_dt;
CREATE TABLE tz_dt (id INT PRIMARY KEY, dt DATETIME);
INSERT INTO tz_dt VALUES (1, '2024-06-01 12:00:00'); -- naive wall clock

DROP TABLE IF EXISTS dt_ts_swap;
CREATE TABLE dt_ts_swap (id INT PRIMARY KEY, dt DATETIME);
INSERT INTO dt_ts_swap VALUES (1, '2024-06-01 04:00:00');

DROP TABLE IF EXISTS t_float_small;
CREATE TABLE t_float_small (id INT PRIMARY KEY, f DOUBLE);
INSERT INTO t_float_small VALUES (1, 1.0);

DROP TABLE IF EXISTS t_float_big;
CREATE TABLE t_float_big (id INT PRIMARY KEY, f DOUBLE);
INSERT INTO t_float_big VALUES (1, 1.0);

-- JSON column type (not TEXT): --normalize-json only applies to real JSON.
-- MySQL re-serializes JSON on read (sorted keys, spacing), so the raw bytes
-- still differ from the dst variant.
DROP TABLE IF EXISTS t_json;
CREATE TABLE t_json (id INT PRIMARY KEY, j JSON);
INSERT INTO t_json VALUES (1, '{"a":1,"b":2}');

DROP TABLE IF EXISTS t_dec;
CREATE TABLE t_dec (id INT PRIMARY KEY, dec_val DECIMAL(10,2));
INSERT INTO t_dec VALUES (1, 1.00), (2, -0.00), (3, 0.10);

DROP TABLE IF EXISTS t_char;
CREATE TABLE t_char (id INT PRIMARY KEY, ch CHAR(10));
INSERT INTO t_char VALUES (1, 'ab');

DROP TABLE IF EXISTS t_bit;
CREATE TABLE t_bit (id INT PRIMARY KEY, b BIT(1));
INSERT INTO t_bit VALUES (1, b'1'), (2, b'0');

DROP TABLE IF EXISTS t_null;
CREATE TABLE t_null (id INT PRIMARY KEY, a VARCHAR(10), b VARCHAR(10));
INSERT INTO t_null VALUES (1, NULL, ''), (2, '', NULL);

DROP TABLE IF EXISTS t_nulltrap;
CREATE TABLE t_nulltrap (id INT PRIMARY KEY, a VARCHAR(10));
INSERT INTO t_nulltrap VALUES (1, NULL);

DROP TABLE IF EXISTS t_enum;
CREATE TABLE t_enum (id INT PRIMARY KEY, e ENUM('a','b'));
INSERT INTO t_enum VALUES (1, 'a'), (2, 'b');

DROP TABLE IF EXISTS t_enumtrap;
CREATE TABLE t_enumtrap (id INT PRIMARY KEY, e ENUM('a','b'));
INSERT INTO t_enumtrap VALUES (1, 'a');

-- Round-8 seeds.

-- TIME in the driver's actual text grammar: negative and fractional values
-- (P1-5)
DROP TABLE IF EXISTS t_time;
CREATE TABLE t_time (id INT PRIMARY KEY, v TIME(6));
INSERT INTO t_time VALUES
  (1, '00:00:00'), (2, '01:02:03'), (3, '-01:02:03'), (4, '838:59:59'),
  (5, '-838:59:59'), (6, '00:00:00.1'), (7, '00:00:00.01'),
  (8, '00:00:00.000001');

-- cross-family numeric (P1-6): the dst declares BIGINT UNSIGNED
DROP TABLE IF EXISTS t_numfam;
CREATE TABLE t_numfam (id INT PRIMARY KEY, v INT);
INSERT INTO t_numfam VALUES (1, 1);

-- large-magnitude finite floats (P0-4)
DROP TABLE IF EXISTS t_float_big2;
CREATE TABLE t_float_big2 (id INT PRIMARY KEY, f DOUBLE);
INSERT INTO t_float_big2 VALUES (1, 1e10), (2, 1e12);

-- JSON values beyond 2^53 (P1-7): MySQL stores both exactly
DROP TABLE IF EXISTS t_jsonbig;
CREATE TABLE t_jsonbig (id INT PRIMARY KEY, j JSON);
INSERT INTO t_jsonbig VALUES (1, '{"n":9007199254740992}');
DROP TABLE IF EXISTS t_jsonbig_ok;
CREATE TABLE t_jsonbig_ok (id INT PRIMARY KEY, j JSON);
INSERT INTO t_jsonbig_ok VALUES (1, '{"a":1}');

-- large BLOB payloads: >64KiB and 1MiB (P0-1)
DROP TABLE IF EXISTS t_bigblob;
CREATE TABLE t_bigblob (id INT PRIMARY KEY, a LONGBLOB, b LONGBLOB);
INSERT INTO t_bigblob VALUES
  (1, REPEAT(0x41, 65536), REPEAT(0x42, 65536)),
  (2, REPEAT(0x43, 1048576), REPEAT(0x44, 1048576));
DROP TABLE IF EXISTS t_bigblob_ok;
CREATE TABLE t_bigblob_ok (id INT PRIMARY KEY, a LONGBLOB);
INSERT INTO t_bigblob_ok VALUES (1, REPEAT(0x45, 1048576));

-- cross-collation string key (P0-3): src is byte-order (utf8mb4_bin), the
-- dst variant is the default case-insensitive collation. Identical data,
-- so the ONLY difference is the key's ordering semantics.
DROP TABLE IF EXISTS t_keycoll;
CREATE TABLE t_keycoll (k VARCHAR(16) COLLATE utf8mb4_bin PRIMARY KEY, v INT);
INSERT INTO t_keycoll VALUES ('Z', 1), ('a', 2);

-- cross-collation key WITH an FK child (P0-3): the parent's data DIFFERS
-- so a sync would want row-level addressing; the child proves a refused
-- sync wrote nothing (no wrong out-of-range delete cascaded).
DROP TABLE IF EXISTS t_keyfk_child;
DROP TABLE IF EXISTS t_keyfk;
CREATE TABLE t_keyfk (k VARCHAR(16) COLLATE utf8mb4_bin PRIMARY KEY, v INT);
INSERT INTO t_keyfk VALUES ('Z', 1), ('a', 2);
CREATE TABLE t_keyfk_child (k VARCHAR(16) COLLATE utf8mb4_bin NOT NULL,
  c INT PRIMARY KEY,
  CONSTRAINT fk_kfc FOREIGN KEY (k) REFERENCES t_keyfk (k) ON DELETE CASCADE);
INSERT INTO t_keyfk_child VALUES ('Z', 100), ('a', 200);

-- cross-timezone TIMESTAMP (P0-2): the 2024-01-01 08:00:00 UTC instant
DROP TABLE IF EXISTS t_timestamp_tz;
CREATE TABLE t_timestamp_tz (id INT PRIMARY KEY, ts TIMESTAMP);
SET time_zone = '+00:00';
INSERT INTO t_timestamp_tz VALUES (1, '2024-01-01 08:00:00');

-- round-9: a JSON number (P0-1): the dst variant holds the JSON STRING
-- of the same digits — the type must survive --normalize-json.
DROP TABLE IF EXISTS t_json_type;
CREATE TABLE t_json_type (id INT PRIMARY KEY, j JSON);
INSERT INTO t_json_type VALUES (1, '{"n":1}');
-- number 1 where the dst variant holds number 1.0: equal only after
-- normalization
DROP TABLE IF EXISTS t_json_type_ok;
CREATE TABLE t_json_type_ok (id INT PRIMARY KEY, j JSON);
INSERT INTO t_json_type_ok VALUES (1, '{"n":1}');

-- round-9: ENUM primary key (P0-2): the member DEFINITION ORDER is
-- reversed on the dst side. Identical data, reversed ordering semantics.
DROP TABLE IF EXISTS t_enumkey;
CREATE TABLE t_enumkey (k ENUM('b','a') PRIMARY KEY, v INT);
INSERT INTO t_enumkey VALUES ('a', 1), ('b', 2);
-- the same drift WITH a data difference: the sync would want row-level
-- addressing, which the incompatible key ordering must refuse
DROP TABLE IF EXISTS t_enumkey_drift;
CREATE TABLE t_enumkey_drift (k ENUM('b','a') PRIMARY KEY, v INT);
INSERT INTO t_enumkey_drift VALUES ('a', 1), ('b', 2);

-- round-9: cross-family numerics over the SHARED canonical payload (P1-3)
DROP TABLE IF EXISTS t_numfam_large;
CREATE TABLE t_numfam_large (id INT PRIMARY KEY, v BIGINT);
INSERT INTO t_numfam_large VALUES (1, 1000000);
DROP TABLE IF EXISTS t_numfam_dec;
CREATE TABLE t_numfam_dec (id INT PRIMARY KEY, v DECIMAL(20,10));
INSERT INTO t_numfam_dec VALUES (1, 0.0000100000);
