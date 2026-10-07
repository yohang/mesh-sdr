-- name: EnqueueOutbox :execrows
INSERT INTO reporting_outbox (network, decoded_message_id, payload, dedup_key, status, attempts, next_attempt_at, created_at)
VALUES (?, ?, ?, ?, 'pending', 0, ?, ?)
ON CONFLICT (network, dedup_key) DO NOTHING;

-- name: ClaimOutbox :many
UPDATE reporting_outbox
SET status = 'in_flight', lease_owner = sqlc.arg(owner), lease_until = sqlc.arg(lease_until)
WHERE id IN (
    SELECT o.id FROM reporting_outbox o
    WHERE o.network = sqlc.arg(network)
      AND ((o.status IN ('pending', 'failed') AND o.next_attempt_at <= sqlc.arg(now))
        OR (o.status = 'in_flight' AND o.lease_until <= sqlc.arg(now)))
    ORDER BY o.id
    LIMIT sqlc.arg(max_rows)
)
RETURNING *;

-- name: SaveOutbox :execrows
UPDATE reporting_outbox
SET status = sqlc.arg(status), attempts = sqlc.arg(attempts), next_attempt_at = sqlc.arg(next_attempt_at),
    lease_owner = sqlc.arg(lease_owner), lease_until = sqlc.arg(lease_until), sent_at = sqlc.arg(sent_at),
    last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id) AND status = 'in_flight' AND lease_owner = sqlc.arg(owner);

-- name: KillWaitingOutbox :execrows
-- An entry waits since it became due (next_attempt_at) or, in flight, since
-- its lease expired (a crashed worker).
UPDATE reporting_outbox
SET status = 'dead', lease_owner = NULL, lease_until = NULL, last_error = sqlc.arg(reason)
WHERE network = sqlc.arg(network)
  AND ((status IN ('pending', 'failed') AND next_attempt_at < sqlc.arg(cutoff))
    OR (status = 'in_flight' AND lease_until < sqlc.arg(cutoff)));

-- name: CountPendingOutbox :one
SELECT COUNT(*) FROM reporting_outbox WHERE network = ? AND status = 'pending';

-- name: KillOverflowOutbox :execrows
UPDATE reporting_outbox
SET status = 'dead', last_error = sqlc.arg(reason)
WHERE id IN (
    SELECT o.id FROM reporting_outbox o
    WHERE o.network = sqlc.arg(network) AND o.status = 'pending'
    ORDER BY o.id DESC
    LIMIT -1 OFFSET sqlc.arg(keep)
);

-- name: PurgeSentOutbox :execrows
DELETE FROM reporting_outbox
WHERE id IN (SELECT o.id FROM reporting_outbox o WHERE o.status = 'sent' AND o.sent_at < sqlc.arg(cutoff) LIMIT sqlc.arg(max_rows));

-- name: PurgeDeadOutbox :execrows
DELETE FROM reporting_outbox
WHERE id IN (SELECT o.id FROM reporting_outbox o WHERE o.status = 'dead' AND o.created_at < sqlc.arg(cutoff) LIMIT sqlc.arg(max_rows));

-- name: OutboxCounts :many
SELECT network, status, COUNT(*) AS total FROM reporting_outbox GROUP BY network, status;

-- name: OutboxTimes :many
SELECT network,
       CAST(COALESCE(MAX(sent_at), 0) AS INTEGER) AS last_sent_at,
       CAST(COALESCE(MIN(CASE WHEN status IN ('pending', 'failed') THEN created_at END), 0) AS INTEGER) AS oldest_due_at
FROM reporting_outbox GROUP BY network;
