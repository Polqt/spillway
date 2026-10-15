CREATE TABLE requests (
    id INTEGER PRIMARY KEY,
    at_ns INTEGER NOT NULL,
    client_app TEXT NOT NULL,
    requested_model TEXT NOT NULL DEFAULT '',
    route TEXT NOT NULL DEFAULT '',
    tier TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    status INTEGER NOT NULL DEFAULT 0,
    error_kind TEXT NOT NULL DEFAULT '',
    stream INTEGER NOT NULL DEFAULT 0,
    cache TEXT NOT NULL DEFAULT '',
    latency_ms INTEGER NOT NULL DEFAULT 0,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    usage_source TEXT NOT NULL DEFAULT '',
    cost_micro_usd INTEGER NOT NULL DEFAULT 0,
    cost_source TEXT NOT NULL DEFAULT '',
    skips_json TEXT NOT NULL DEFAULT '[]'
);

CREATE INDEX requests_at_ns ON requests(at_ns DESC, id DESC);

CREATE TABLE attempts (
    id INTEGER PRIMARY KEY,
    request_id INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    outcome TEXT NOT NULL,
    latency_ms INTEGER NOT NULL DEFAULT 0,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    usage_source TEXT NOT NULL DEFAULT '',
    cost_micro_usd INTEGER NOT NULL DEFAULT 0,
    cost_source TEXT NOT NULL DEFAULT ''
);

CREATE INDEX attempts_request_id ON attempts(request_id, id);
