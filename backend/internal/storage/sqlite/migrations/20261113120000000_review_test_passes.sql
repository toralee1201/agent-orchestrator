-- +goose Up
-- +goose StatementBegin
-- A worker records that unit tests passed for one PR head. ao review trigger
-- refuses to start a reviewer until this row exists for that session and SHA.
CREATE TABLE review_test_passes (
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    target_sha TEXT NOT NULL,
    passed_at  TIMESTAMP NOT NULL,
    PRIMARY KEY (session_id, target_sha)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS review_test_passes;
-- +goose StatementEnd
