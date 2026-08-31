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
