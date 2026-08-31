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
