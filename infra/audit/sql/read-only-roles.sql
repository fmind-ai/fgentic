-- Read-only collector roles for the content-bounded identity audit (issue #418, ADR 0018 gate/Task 4).
--
-- Each role has NOLOGIN and column-level SELECT on ONLY the exact content-free columns the pinned
-- projector needs (infra/audit/base/records.py). It is never granted SELECT on event content,
-- unrecognized keys, credentials, or any payload column, so a compromised collector cannot read a
-- secret it was never granted. check:identity-audit pins every grant here to an exact per-table
-- column allowlist (the events grant tied to the projector's MATRIX_EVENT_SOURCE_COLUMNS, the MAS
-- grants to explicit physical-column allowlists), so a widened, drifted, or whole-table grant — or
-- any forbidden column — fails the gate.
--
-- These are the canonical grant definitions; the opt-in Kustomize component (a follow-up #418 task)
-- provisions them through CloudNativePG. The password/credential is supplied out of band, never here.

-- --- Synapse: fgentic.matrix_event.v1 -------------------------------------------------------------
-- Granted exactly MATRIX_EVENT_SOURCE_COLUMNS; `content`, `unrecognized_keys`, and every other
-- events column are deliberately absent.
CREATE ROLE fgentic_audit_matrix_reader NOLOGIN;
GRANT CONNECT ON DATABASE synapse TO fgentic_audit_matrix_reader;
GRANT USAGE ON SCHEMA public TO fgentic_audit_matrix_reader;
GRANT SELECT (
  event_id,
  type,
  room_id,
  sender,
  origin_server_ts,
  received_ts,
  stream_ordering,
  outlier,
  rejection_reason
) ON public.events TO fgentic_audit_matrix_reader;

-- --- MAS: fgentic.mas_authentication.v1 -----------------------------------------------------------
-- The projector consumes a post-join row; the role is granted only the exact columns each joined
-- table contributes. `user_password_id` / `upstream_oauth_authorization_session_id` drive the method
-- discrimination (which FK is set); it is never granted the password hash, email, IP, or User-Agent.
CREATE ROLE fgentic_audit_mas_reader NOLOGIN;
GRANT CONNECT ON DATABASE mas TO fgentic_audit_mas_reader;
GRANT USAGE ON SCHEMA public TO fgentic_audit_mas_reader;
GRANT SELECT (
  user_session_authentication_id,
  user_session_id,
  created_at,
  user_password_id,
  upstream_oauth_authorization_session_id
) ON public.user_session_authentications TO fgentic_audit_mas_reader;
GRANT SELECT (user_session_id, user_id) ON public.user_sessions TO fgentic_audit_mas_reader;
GRANT SELECT (user_id, username) ON public.users TO fgentic_audit_mas_reader;
-- Resolves upstream_provider_id for the upstream_oidc method: only the join key and the provider id,
-- never the OAuth authorization code, tokens, or redirect URI on this table.
GRANT SELECT (upstream_oauth_authorization_session_id, upstream_oauth_provider_id) ON public.upstream_oauth_authorization_sessions TO fgentic_audit_mas_reader;
