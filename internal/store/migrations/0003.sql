CREATE TABLE evaluation_runs (
    run_id TEXT PRIMARY KEY,
    created_ns INTEGER NOT NULL,
    corpus_sha256 TEXT NOT NULL,
    summary_json TEXT NOT NULL
);

CREATE INDEX evaluation_runs_created_ns ON evaluation_runs(created_ns DESC, run_id DESC);
