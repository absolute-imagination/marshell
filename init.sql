-- Marshell Network (self-hosted) — core schema only.
-- No wallets, profiles, billing, or SaaS console tables.

CREATE SCHEMA IF NOT EXISTS extensions;
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA extensions;

CREATE TABLE public.subnets (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name            text NOT NULL,
    join_token_hash text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE public.agents (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subnet_id      uuid NOT NULL REFERENCES public.subnets(id) ON DELETE CASCADE,
    name           text NOT NULL,
    status         text NOT NULL DEFAULT 'online',
    last_seen_at   timestamptz,
    agent_card     jsonb,
    usage_allowed  text,
    usage_denied   text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (subnet_id, name)
);

CREATE TABLE public.agent_credentials (
    agent_id   uuid PRIMARY KEY REFERENCES public.agents(id) ON DELETE CASCADE,
    key_hash   text NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE public.agent_messages (
    id                text PRIMARY KEY,
    subnet_id         uuid NOT NULL REFERENCES public.subnets(id) ON DELETE CASCADE,
    from_agent_id     uuid NOT NULL REFERENCES public.agents(id) ON DELETE CASCADE,
    to_agent_id       uuid NOT NULL REFERENCES public.agents(id) ON DELETE CASCADE,
    from_name         text NOT NULL,
    to_name           text NOT NULL,
    body              text NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    correlation_id    text,
    client_message_id text
);

CREATE UNIQUE INDEX agent_messages_client_message_id_uidx
    ON public.agent_messages (from_agent_id, client_message_id)
    WHERE client_message_id IS NOT NULL AND client_message_id <> '';

CREATE INDEX agent_messages_created_idx ON public.agent_messages (created_at DESC);
CREATE INDEX agent_messages_peers_idx ON public.agent_messages (from_agent_id, to_agent_id, created_at DESC);

CREATE TABLE public.subnet_links (
    from_subnet_id uuid NOT NULL REFERENCES public.subnets(id) ON DELETE CASCADE,
    to_subnet_id   uuid NOT NULL REFERENCES public.subnets(id) ON DELETE CASCADE,
    status         text NOT NULL DEFAULT 'active',
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (from_subnet_id, to_subnet_id),
    CHECK (from_subnet_id <> to_subnet_id)
);

CREATE TABLE public.agent_permissions (
    agent_id       uuid NOT NULL REFERENCES public.agents(id) ON DELETE CASCADE,
    permission_key text NOT NULL,
    can_read       boolean NOT NULL DEFAULT true,
    can_write      boolean NOT NULL DEFAULT true,
    PRIMARY KEY (agent_id, permission_key)
);

CREATE TABLE public.agent_events (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id   uuid NOT NULL REFERENCES public.agents(id) ON DELETE CASCADE,
    subnet_id  uuid NOT NULL REFERENCES public.subnets(id) ON DELETE CASCADE,
    kind       text NOT NULL,
    summary    text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX agent_events_agent_idx ON public.agent_events (agent_id, created_at DESC);

CREATE TABLE public.approval_requests (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    subnet_id     uuid NOT NULL REFERENCES public.subnets(id) ON DELETE CASCADE,
    from_agent_id uuid NOT NULL REFERENCES public.agents(id) ON DELETE CASCADE,
    action_key    text NOT NULL,
    summary       text NOT NULL DEFAULT '',
    payload       jsonb NOT NULL DEFAULT '{}'::jsonb,
    status        text NOT NULL DEFAULT 'pending',
    created_at    timestamptz NOT NULL DEFAULT now(),
    resolved_at   timestamptz
);

-- Default local subnet. Join token (plaintext): msk_local_dev_join_token_change_me
INSERT INTO public.subnets (id, name, join_token_hash)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    'local',
    extensions.crypt('msk_local_dev_join_token_change_me', extensions.gen_salt('bf'))
);
