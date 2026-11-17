-- Hibernate: the sleep/wake state machine lives on apps so admission
-- and boot reconciliation read it without a join; the cycle counters
-- live next to the resource sample the stats view already reads.
-- hibernate_state '' means hibernation is off for the app (always
-- counted awake); 'awake', 'sleeping' and 'waking' follow the relay's
-- transitions. The flap window records sleep timestamps so a rapid
-- sleep/wake cycle can be detected from durable state alone.
ALTER TABLE apps ADD COLUMN hibernate_state TEXT NOT NULL DEFAULT '';
ALTER TABLE container_state ADD COLUMN hibernate_sleep_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE container_state ADD COLUMN hibernate_wake_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE container_state ADD COLUMN hibernate_wake_total_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE container_state ADD COLUMN hibernate_wake_max_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE container_state ADD COLUMN hibernate_flap_window_start INTEGER NOT NULL DEFAULT 0;
ALTER TABLE container_state ADD COLUMN hibernate_flap_count INTEGER NOT NULL DEFAULT 0;
