CREATE TABLE IF NOT EXISTS organizers (
    id         text PRIMARY KEY,
    name       text   NOT NULL,
    balance    bigint NOT NULL CHECK (balance >= 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS users (
    id         text PRIMARY KEY,
    name       text   NOT NULL,
    balance    bigint NOT NULL CHECK (balance >= 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS events (
    id           text PRIMARY KEY,
    name         text NOT NULL,
    organizer_id text NOT NULL REFERENCES organizers (id),
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- seats.status is the anchor of all per-seat concurrency control:
-- every state transition first takes SELECT ... FOR UPDATE on the seat row.
CREATE TABLE IF NOT EXISTS seats (
    event_id text   NOT NULL REFERENCES events (id),
    number   text   NOT NULL,
    ord      int    NOT NULL,
    price    bigint NOT NULL CHECK (price > 0),
    status   text   NOT NULL DEFAULT 'available'
             CHECK (status IN ('available', 'reserved', 'payment_pending', 'purchased')),
    PRIMARY KEY (event_id, number)
);

CREATE TABLE IF NOT EXISTS reservations (
    id          text PRIMARY KEY,
    user_id     text NOT NULL REFERENCES users (id),
    event_id    text NOT NULL,
    seat_number text NOT NULL,
    status      text NOT NULL
                CHECK (status IN ('reserved', 'payment_pending', 'purchased',
                                  'canceled', 'expired', 'payment_failed')),
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at  timestamptz NOT NULL,
    finished_at timestamptz,
    FOREIGN KEY (event_id, seat_number) REFERENCES seats (event_id, number)
);

-- Second line of defence: at most one live reservation per seat, enforced by the database.
CREATE UNIQUE INDEX IF NOT EXISTS reservations_one_live_per_seat
    ON reservations (event_id, seat_number)
    WHERE status IN ('reserved', 'payment_pending', 'purchased');

CREATE INDEX IF NOT EXISTS reservations_due
    ON reservations (expires_at) WHERE status = 'reserved';

CREATE TABLE IF NOT EXISTS purchases (
    id             text PRIMARY KEY,
    reservation_id text NOT NULL UNIQUE REFERENCES reservations (id),
    status         text NOT NULL
                   CHECK (status IN ('payment_pending', 'purchased', 'failed', 'timed_out')),
    started_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at    timestamptz
);

CREATE TABLE IF NOT EXISTS payments (
    id             text PRIMARY KEY,
    purchase_id    text   NOT NULL UNIQUE REFERENCES purchases (id),
    user_id        text   NOT NULL REFERENCES users (id),
    organizer_id   text   NOT NULL REFERENCES organizers (id),
    amount         bigint NOT NULL CHECK (amount > 0),
    status         text   NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed', 'timed_out')),
    failure_reason text,
    created_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at     timestamptz NOT NULL,
    completed_at   timestamptz
);

CREATE INDEX IF NOT EXISTS payments_due
    ON payments (expires_at) WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS transitions (
    id          bigserial PRIMARY KEY,
    entity      text NOT NULL,
    entity_id   text NOT NULL,
    from_status text NOT NULL,
    to_status   text NOT NULL,
    reason      text NOT NULL DEFAULT '',
    at          timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS transitions_entity ON transitions (entity_id, id);
