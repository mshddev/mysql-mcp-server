-- Throwaway dev database mirroring the staging setup: a couple of tables
-- and a SELECT-only user, so guardrails can be tested honestly.
DROP DATABASE IF EXISTS mcp_dev;
CREATE DATABASE mcp_dev CHARACTER SET utf8mb4;
USE mcp_dev;

CREATE TABLE users (
  id INT PRIMARY KEY AUTO_INCREMENT,
  name VARCHAR(100) NOT NULL,
  email VARCHAR(150),
  phone VARCHAR(30),
  avatar BLOB,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE bookings (
  id INT PRIMARY KEY AUTO_INCREMENT,
  user_id INT NOT NULL,
  room_name VARCHAR(100) NOT NULL,
  price DECIMAL(12,2) NOT NULL,
  status ENUM('pending','paid','cancelled') DEFAULT 'pending',
  booked_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO users (name, email, phone, avatar) VALUES
  ('Andi Wijaya', 'andi@example.com', '081234567890', X'89504E470D0A'),
  ('Budi Santoso', 'budi@example.com', NULL, NULL),
  ('Citra Lestari', NULL, '085612345678', NULL);

INSERT INTO bookings (user_id, room_name, price, status) VALUES
  (1, 'Kos Melati A1', 1500000.00, 'paid'),
  (1, 'Kos Melati A1', 1500000.00, 'pending'),
  (2, 'Kos Anggrek B2', 2250000.50, 'paid'),
  (3, 'Kos Mawar C3', 900000.00, 'cancelled');

-- A view that renames a PII column. What the wire protocol reports as the
-- column's origin here differs between MariaDB (the new name — masking rules
-- for the base column can't see through it) and MySQL variants; the masking
-- integration tests pin whichever behavior the server under test exhibits.
CREATE VIEW user_contacts AS
  SELECT id, name AS contact_name, phone AS contact FROM users;

-- Big table to exercise the row/byte caps (~10k rows via cross join).
CREATE TABLE big (
  id INT PRIMARY KEY AUTO_INCREMENT,
  filler VARCHAR(255)
);
INSERT INTO big (filler)
SELECT CONCAT('row-', a.id, '-', b.id, '-', REPEAT('x', 200))
FROM (SELECT id FROM bookings) a
CROSS JOIN (
  SELECT b1.id + 4 * b2.id + 16 * b3.id + 64 * b4.id + 256 * b5.id AS id
  FROM (SELECT 0 id UNION SELECT 1 UNION SELECT 2 UNION SELECT 3) b1,
       (SELECT 0 id UNION SELECT 1 UNION SELECT 2 UNION SELECT 3) b2,
       (SELECT 0 id UNION SELECT 1 UNION SELECT 2 UNION SELECT 3) b3,
       (SELECT 0 id UNION SELECT 1 UNION SELECT 2 UNION SELECT 3) b4,
       (SELECT 0 id UNION SELECT 1 UNION SELECT 2 UNION SELECT 3) b5
) b;

-- Both host variants: default MariaDB installs keep anonymous ''@'localhost'
-- users that shadow '%' users on local connections.
CREATE USER IF NOT EXISTS 'mcp_readonly'@'%' IDENTIFIED BY 'devpassword';
CREATE USER IF NOT EXISTS 'mcp_readonly'@'localhost' IDENTIFIED BY 'devpassword';
ALTER USER 'mcp_readonly'@'%' IDENTIFIED BY 'devpassword';
ALTER USER 'mcp_readonly'@'localhost' IDENTIFIED BY 'devpassword';
GRANT SELECT ON mcp_dev.* TO 'mcp_readonly'@'%';
GRANT SELECT ON mcp_dev.* TO 'mcp_readonly'@'localhost';
FLUSH PRIVILEGES;
