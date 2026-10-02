-- channel_monitor_mode now also accepts 'hybrid' in the application runtime.
-- Preserve every stored v1/v2 selection, feature flag, and monitor enabled flag.
-- A passive deployment must opt into hybrid before legacy active probes resume.
-- The setting is an unconstrained string, so no schema or data rewrite is needed.
SELECT 1;
