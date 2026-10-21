-- Check effective settings in this connection before touching the catalog.
-- Poolers that discard PGOPTIONS must fail closed.
DO $backup_session$
BEGIN
  IF current_setting('default_transaction_read_only') <> 'on'
    OR current_setting('transaction_read_only') <> 'on'
    OR current_setting('statement_timeout')::interval <> interval '5 seconds'
  THEN
    RAISE EXCEPTION 'Unsafe backup session settings';
  END IF;
END;
$backup_session$;

SELECT json_build_object(
  'server_version', current_setting('server_version'),
  'server_version_num', current_setting('server_version_num')::int,
  'schemas', (
    SELECT coalesce(
      json_agg(
        json_build_object('oid', oid::bigint, 'name', nspname)
        ORDER BY oid
      ),
      '[]'::json
    )
    FROM pg_namespace
    WHERE nspname !~ '^pg_' AND nspname <> 'information_schema'
  ),
  'tables', (
    SELECT coalesce(
      json_agg(
        json_build_object(
          'oid', c.oid::bigint,
          'schema', n.nspname,
          'table', c.relname,
          'kind', c.relkind,
          'rls', c.relrowsecurity,
          'select', has_table_privilege(c.oid, 'SELECT'),
          'parents', (
            SELECT coalesce(json_agg(inhparent::bigint ORDER BY inhparent), '[]'::json)
            FROM pg_inherits
            WHERE inhrelid = c.oid
          )
        )
        ORDER BY c.oid
      ),
      '[]'::json
    )
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE c.relkind IN ('r', 'p', 'f')
      AND n.nspname !~ '^pg_'
      AND n.nspname <> 'information_schema'
  )
);
