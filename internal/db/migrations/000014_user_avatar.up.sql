-- Feedback wave 1, item 8: OIDC avatars.
--
-- `avatar_url` is the `picture` claim, as asserted by the IdP at last login.
-- `profile` is already in the default OIDC scope set, so this needs no config
-- change -- only somewhere to put what was already being sent and discarded.
--
-- NULL means "the IdP asserted none" (or dev login, which asserts nothing at
-- all) -- never "unknown". The reconciler writes this column unconditionally
-- on every login, the same as email and display_name.
--
-- The CHECK is the structural half of the https-only guarantee. `verify()`
-- (internal/auth/oidc.go) already filters the claim to https-only before it
-- reaches the reconciler -- that is the friendly, fast-fail path. This is the
-- guarantee: even a future caller that forgets the filter, or writes this
-- column from some other path, cannot store a `data:`/`http:`/scheme-relative
-- value that the console would then render into an <img src>.
ALTER TABLE users ADD COLUMN avatar_url text
    CONSTRAINT users_avatar_url_https CHECK (avatar_url IS NULL OR avatar_url LIKE 'https://%');
