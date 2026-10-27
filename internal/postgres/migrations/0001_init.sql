-- Organizers receive the money for sold seats.
CREATE TABLE organizers (
    id         text PRIMARY KEY,
    name       text   NOT NULL,
    balance    bigint NOT NULL CHECK (balance >= 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- Accounts. Buyers have role 'user' and a test balance; 'admin' manages the
-- catalogue; 'provider' models the payment system that sends callbacks.
CREATE TABLE users (
    id            text PRIMARY KEY,
    name          text   NOT NULL,
    login         text   NOT NULL,
    password_hash text   NOT NULL,
    role          text   NOT NULL CHECK (role IN ('user', 'admin', 'provider')),
    balance       bigint NOT NULL CHECK (balance >= 0),
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE UNIQUE INDEX users_login_key ON users (lower(login));

-- Refresh tokens are stored as SHA-256 hashes; rotation links old -> new.
CREATE TABLE refresh_tokens (
    id          text PRIMARY KEY,
    user_id     text  NOT NULL REFERENCES users (id),
    token_hash  bytea NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz,
    replaced_by text REFERENCES refresh_tokens (id)
);

CREATE INDEX refresh_tokens_active_by_user ON refresh_tokens (user_id) WHERE revoked_at IS NULL;

CREATE TABLE events (
    id           text PRIMARY KEY,
    name         text NOT NULL,
    organizer_id text NOT NULL REFERENCES organizers (id),
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- seats.status is the anchor of per-seat concurrency control: every state
-- change of a seat starts with SELECT ... FOR UPDATE on its row.
CREATE TABLE seats (
    event_id   text   NOT NULL REFERENCES events (id),
    number     text   NOT NULL,
    seat_order int    NOT NULL,
    price      bigint NOT NULL CHECK (price > 0),
    status     text   NOT NULL DEFAULT 'available'
               CHECK (status IN ('available', 'reserved', 'payment_pending', 'purchased')),
    PRIMARY KEY (event_id, number)
);

CREATE TABLE reservations (
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

-- Second line of defence: the database itself refuses a second live
-- reservation of the same seat, whatever the application does.
CREATE UNIQUE INDEX reservations_one_live_per_seat
    ON reservations (event_id, seat_number)
    WHERE status IN ('reserved', 'payment_pending', 'purchased');

CREATE INDEX reservations_due ON reservations (expires_at) WHERE status = 'reserved';
CREATE INDEX reservations_by_user ON reservations (user_id);

CREATE TABLE purchases (
    id             text PRIMARY KEY,
    reservation_id text NOT NULL UNIQUE REFERENCES reservations (id),
    status         text NOT NULL
                   CHECK (status IN ('payment_pending', 'purchased', 'failed', 'timed_out')),
    started_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at    timestamptz
);

CREATE TABLE payments (
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

CREATE INDEX payments_due ON payments (expires_at) WHERE status = 'pending';

-- History of every state change.
CREATE TABLE transitions (
    id          bigserial PRIMARY KEY,
    entity      text NOT NULL,
    entity_id   text NOT NULL,
    from_status text NOT NULL,
    to_status   text NOT NULL,
    reason      text NOT NULL DEFAULT '',
    at          timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX transitions_by_entity ON transitions (entity_id, id);
