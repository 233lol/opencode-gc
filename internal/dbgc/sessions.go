package dbgc

import (
	"context"
)

// SessionStat is the storage footprint and graph information for a session.
type SessionStat struct {
	Messages         int64
	MessageBytes     int64
	BlobBytes        int64
	PartBytes        int64
	InstructionBytes int64
	TotalBytes       int64
	Children         int64
}

// LoadSessionStats calculates per-session sizes from messages, blobs, parts and
// instruction state.
func LoadSessionStats(ctx context.Context, d *DB) (map[string]SessionStat, error) {
	out := make(map[string]SessionStat)

	for id, v := range sessionMessageStats(ctx, d) {
		st := out[id]
		st.Messages = v[0]
		st.MessageBytes = v[1]
		out[id] = st
	}

	if d.HasTable("blob_ref") && d.HasTable("blob") {
		rows, err := d.sql.QueryContext(ctx, `
			SELECT session_id, sum(bytes) FROM (
				SELECT DISTINCT br.session_id, b.hash, b.bytes
				FROM blob_ref br JOIN blob b ON b.hash = br.hash
			) GROUP BY session_id`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var n int64
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return nil, err
			}
			st := out[id]
			st.BlobBytes = n
			out[id] = st
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	if d.HasTable("session_part") {
		rows, err := d.sql.QueryContext(ctx, `
			SELECT session_id, coalesce(sum(length(data)),0)
			FROM session_part GROUP BY session_id`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var n int64
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return nil, err
			}
			st := out[id]
			st.PartBytes = n
			out[id] = st
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	if d.HasTable("instruction_state") {
		rows, err := d.sql.QueryContext(ctx, `
			SELECT session_id, coalesce(sum(length(initial_values) + length(current_values)),0)
			FROM instruction_state GROUP BY session_id`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var n int64
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return nil, err
			}
			st := out[id]
			st.InstructionBytes = n
			out[id] = st
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	if d.HasTable("session_v2") {
		rows, err := d.sql.QueryContext(ctx, `
			SELECT parent_id, count(*) FROM session_v2
			WHERE parent_id IS NOT NULL AND parent_id <> ''
			GROUP BY parent_id`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var n int64
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return nil, err
			}
			st := out[id]
			st.Children = n
			out[id] = st
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	for id, st := range out {
		st.TotalBytes = st.MessageBytes + st.BlobBytes + st.PartBytes + st.InstructionBytes
		out[id] = st
	}
	return out, nil
}

// CountSessions returns how many of the given session IDs still exist.
func (d *DB) CountSessions(ctx context.Context, ids []string) (int64, error) {
	var total int64
	for _, batch := range chunks(ids, 500) {
		ph := placeholders(len(batch))
		args := make([]any, 0, len(batch))
		for _, id := range batch {
			args = append(args, id)
		}
		var n int64
		if err := d.sql.QueryRowContext(ctx,
			`SELECT count(*) FROM session_v2 WHERE id IN (`+ph+`)`, args...).Scan(&n); err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// OrphanRules exposes the referential cleanup rules for targeted session
// deletion.
func OrphanRules(d *DB) []OrphanRule {
	return buildOrphanRules(d)
}
