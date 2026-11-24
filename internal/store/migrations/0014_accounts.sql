-- Accounts reached over the network: their credentials, sessions and enrollment codes.
ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0,1));
ALTER TABLE users ADD COLUMN password_allowed INTEGER NOT NULL DEFAULT 0 CHECK (password_allowed IN (0,1));
ALTER TABLE users ADD COLUMN webauthn_handle BLOB CHECK (webauthn_handle IS NULL OR length(webauthn_handle) = 64);
CREATE UNIQUE INDEX users_webauthn_handle ON users(webauthn_handle) WHERE webauthn_handle IS NOT NULL;

CREATE TABLE credentials (
    id TEXT PRIMARY KEY NOT NULL,
    user TEXT NOT NULL REFERENCES users(id),
    kind TEXT NOT NULL CHECK (kind IN ('passkey','password')),
    name TEXT NOT NULL,
    credential_id BLOB UNIQUE,
    public_key BLOB,
    sign_count INTEGER CHECK (sign_count IS NULL OR sign_count BETWEEN 0 AND 4294967295),
    aaguid BLOB,
    flags INTEGER CHECK (flags IS NULL OR flags BETWEEN 0 AND 255),
    transports TEXT,
    password_hash TEXT,
    created TEXT NOT NULL,
    last_used TEXT,
    CHECK ((kind = 'passkey' AND credential_id IS NOT NULL AND public_key IS NOT NULL AND password_hash IS NULL)
        OR (kind = 'password' AND password_hash IS NOT NULL AND credential_id IS NULL AND public_key IS NULL))
);
CREATE INDEX credentials_user ON credentials(user);
CREATE UNIQUE INDEX credentials_one_password ON credentials(user) WHERE kind = 'password';

CREATE TABLE sessions (
    id TEXT PRIMARY KEY NOT NULL,
    token_hash BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    user TEXT NOT NULL REFERENCES users(id),
    credential TEXT NOT NULL REFERENCES credentials(id),
    created TEXT NOT NULL,
    last_used TEXT NOT NULL,
    address TEXT NOT NULL
);
CREATE INDEX sessions_user ON sessions(user);
CREATE INDEX sessions_credential ON sessions(credential);

CREATE TABLE enrollment_codes (
    code_hash BLOB PRIMARY KEY NOT NULL CHECK (length(code_hash) = 32),
    user TEXT NOT NULL REFERENCES users(id),
    created TEXT NOT NULL,
    expires TEXT NOT NULL,
    used TEXT
);
CREATE INDEX enrollment_codes_user ON enrollment_codes(user);
