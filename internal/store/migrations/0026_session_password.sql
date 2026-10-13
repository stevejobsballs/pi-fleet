-- Which password a sign-in was made with (a hash of the user's verifier
-- then): changing or resetting the password ends every other sign-in.
-- Sign-ins from before this have none, and end.
ALTER TABLE sessions ADD COLUMN password_tag TEXT NOT NULL DEFAULT '';
