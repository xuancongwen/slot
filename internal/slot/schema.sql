PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=5000;

CREATE TABLE IF NOT EXISTS users (
 id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE, password TEXT NOT NULL,
 name TEXT NOT NULL, slug TEXT NOT NULL UNIQUE,
 enabled INTEGER NOT NULL DEFAULT 0, write_calendar INTEGER,
 default_location INTEGER REFERENCES locations(id) ON DELETE SET NULL,
 created INTEGER NOT NULL
);
-- Where a host is willing to meet. Guests pick one or supply their own.
-- A 'meet' location has no detail: Google creates a fresh Meet link per booking.
CREATE TABLE IF NOT EXISTS locations (
 id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 kind TEXT NOT NULL CHECK(kind IN ('meet','custom')),
 label TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '', position INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS meeting_types (
 id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 slug TEXT NOT NULL, name TEXT NOT NULL, timezone TEXT NOT NULL DEFAULT 'UTC',
 days TEXT NOT NULL DEFAULT '12345', start_min INTEGER NOT NULL DEFAULT 540,
 end_min INTEGER NOT NULL DEFAULT 1020, duration INTEGER NOT NULL DEFAULT 30,
 buffer INTEGER NOT NULL DEFAULT 0, notice INTEGER NOT NULL DEFAULT 120,
 horizon INTEGER NOT NULL DEFAULT 30, active INTEGER NOT NULL DEFAULT 1,
 -- Hold each booking as a request until the host approves it; only then is the guest invited.
 approval INTEGER NOT NULL DEFAULT 0,
 all_locations INTEGER NOT NULL DEFAULT 1, guest_location INTEGER NOT NULL DEFAULT 1,
 UNIQUE(user_id,slug)
);
-- The locations a meeting type offers when all_locations is off. guest_location then
-- says whether guests may also type their own; with all_locations on, they always may.
CREATE TABLE IF NOT EXISTS meeting_type_locations (
 meeting_type_id INTEGER NOT NULL REFERENCES meeting_types(id) ON DELETE CASCADE,
 location_id INTEGER NOT NULL REFERENCES locations(id) ON DELETE CASCADE,
 PRIMARY KEY(meeting_type_id,location_id)
);
CREATE TABLE IF NOT EXISTS sessions (
 token TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 expires INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS accounts (
 id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id),
 identity TEXT NOT NULL, token BLOB NOT NULL, UNIQUE(user_id,identity)
);
CREATE TABLE IF NOT EXISTS calendars (
 id INTEGER PRIMARY KEY, account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
 google_id TEXT NOT NULL, name TEXT NOT NULL, role TEXT NOT NULL,
 check_busy INTEGER NOT NULL DEFAULT 0, UNIQUE(account_id,google_id)
);
CREATE TABLE IF NOT EXISTS oauth_states (
 state TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id),
 session TEXT NOT NULL, verifier TEXT NOT NULL, expires INTEGER NOT NULL
);
-- A host's days off: every date from first_day through last_day, as YYYY-MM-DD.
CREATE TABLE IF NOT EXISTS blocks (
 id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id),
 first_day TEXT NOT NULL, last_day TEXT NOT NULL, CHECK(last_day >= first_day)
);
CREATE TABLE IF NOT EXISTS bookings (
 id TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id),
 calendar_id INTEGER NOT NULL REFERENCES calendars(id),
 guest_name TEXT NOT NULL, guest_email TEXT NOT NULL,
 start INTEGER NOT NULL, end INTEGER NOT NULL, block_start INTEGER NOT NULL, block_end INTEGER NOT NULL,
 title TEXT NOT NULL, location TEXT NOT NULL, meet INTEGER NOT NULL DEFAULT 0, timezone TEXT NOT NULL,
 guest_timezone TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '',
 manage_token TEXT NOT NULL UNIQUE,
 status TEXT NOT NULL CHECK(status IN ('requested','pending','confirmed','cancel_pending','cancelled','declined','failed')),
 created INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
 -- When a confirmed booking's Google event was last compared with Slot's copy.
 checked INTEGER NOT NULL DEFAULT 0,
 CHECK(end > start), CHECK(block_end > block_start)
);
CREATE INDEX IF NOT EXISTS bookings_host_time ON bookings(user_id,block_start,block_end);
CREATE INDEX IF NOT EXISTS bookings_retry ON bookings(status,next_attempt);
CREATE TRIGGER IF NOT EXISTS no_overlapping_bookings BEFORE INSERT ON bookings
WHEN NEW.status IN ('requested','pending','confirmed','cancel_pending')
BEGIN
 SELECT RAISE(ABORT,'slot_overlap') WHERE EXISTS (
  SELECT 1 FROM bookings WHERE (user_id=NEW.user_id OR calendar_id IN (
   SELECT id FROM calendars WHERE google_id=(SELECT google_id FROM calendars WHERE id=NEW.calendar_id)
  ))
  AND status IN ('requested','pending','confirmed','cancel_pending')
  AND block_start < NEW.block_end AND block_end > NEW.block_start
 );
END;
