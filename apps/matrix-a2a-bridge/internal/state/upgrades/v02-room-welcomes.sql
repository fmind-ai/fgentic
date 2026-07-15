-- v1 -> v2: Permanent content-free room welcome markers
-- only: postgres until "end only"

CREATE TABLE bridge_room_welcomes (
	room_id     TEXT PRIMARY KEY CHECK (room_id <> ''),
	welcome_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- end only postgres
