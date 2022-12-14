-- Currently there is no schema versioning/management
-- This entire script is intended to run against an empty DB as an initialization step
-- It is *SAFE* to run it against a live database with existing data
--
--   psql service=XYZ < misc/pg_schema.sql
--

CREATE SCHEMA IF NOT EXISTS naive;

CREATE OR REPLACE
  FUNCTION naive.ts_from_epoch(INTEGER) RETURNS TIMESTAMP WITH TIME ZONE
LANGUAGE sql PARALLEL SAFE IMMUTABLE STRICT AS $$
  SELECT TIMEZONE( 'UTC', TO_TIMESTAMP( $1 * 30::BIGINT + 1598306400 ) )
$$;

CREATE OR REPLACE
  FUNCTION naive.epoch_from_ts(TIMESTAMP WITH TIME ZONE) RETURNS INTEGER
LANGUAGE sql PARALLEL SAFE IMMUTABLE STRICT AS $$
  SELECT ( EXTRACT( EPOCH FROM $1 )::BIGINT - 1598306400 ) / 30
$$;

CREATE OR REPLACE
  FUNCTION naive.looks_like_cid_v1(TEXT) RETURNS BOOLEAN
    LANGUAGE sql PARALLEL SAFE IMMUTABLE STRICT
AS $$
  SELECT SUBSTRING( $1 FROM 1 FOR 2 ) = 'ba'
$$;

CREATE OR REPLACE
  FUNCTION naive.looks_like_cid(TEXT) RETURNS BOOLEAN
    LANGUAGE sql PARALLEL SAFE IMMUTABLE STRICT
AS $$
  SELECT ( SUBSTRING( $1 FROM 1 FOR 2 ) = 'ba' OR SUBSTRING( $1 FROM 1 FOR 2 ) = 'Qm' )
$$;


CREATE OR REPLACE
  FUNCTION naive.update_entry_timestamp() RETURNS TRIGGER
    LANGUAGE plpgsql
AS $$
BEGIN
  NEW.entry_last_updated = NOW();
  RETURN NEW;
END;
$$;


CREATE TABLE IF NOT EXISTS naive.global(
  singleton_row BOOL NOT NULL UNIQUE CONSTRAINT single_row_in_table CHECK ( singleton_row IS TRUE ),
  metadata JSONB NOT NULL
);
INSERT INTO naive.global ( singleton_row, metadata ) VALUES ( true, '{ "schema_version":{ "major": 1, "minor": 0 } }' ) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS naive.pieces (
  piece_id BIGINT NOT NULL UNIQUE,
  piece_cid TEXT NOT NULL UNIQUE CONSTRAINT piece_valid_pcid CHECK ( naive.looks_like_cid_v1( piece_cid ) ),
  proven_log2_size SMALLINT CONSTRAINT proven_valid_size CHECK ( proven_log2_size > 0 ),
  piece_meta JSONB NOT NULL DEFAULT '{}'
);
CREATE OR REPLACE
  FUNCTION naive.prefill_piece_id() RETURNS TRIGGER
    LANGUAGE plpgsql
AS $$
BEGIN
  NEW.piece_id = ( SELECT COALESCE( MIN( piece_id ), 0 ) - 1 FROM naive.pieces );
  RETURN NEW;
END;
$$;
CREATE OR REPLACE TRIGGER trigger_fill_next_piece_id
  BEFORE INSERT ON naive.pieces
  FOR EACH ROW
  WHEN ( NEW.piece_id IS NULL )
  EXECUTE PROCEDURE naive.prefill_piece_id()
;


CREATE TABLE IF NOT EXISTS naive.clients (
  client_id INTEGER UNIQUE NOT NULL,
  client_address TEXT UNIQUE CONSTRAINT client_valid_address CHECK ( SUBSTRING( client_address FROM 1 FOR 2 ) IN ( 'f1', 'f3', 'f4' ) ),
  client_meta JSONB NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS naive.providers (
  provider_id INTEGER UNIQUE NOT NULL,
  provider_meta JSONB NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS naive.providers_info (
  provider_id INTEGER UNIQUE NOT NULL REFERENCES naive.providers ( provider_id ),
  provider_last_polled TIMESTAMP WITH TIME ZONE NOT NULL,
  info_last_updated TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
  info_dialing_took_msecs INTEGER,
  info_dialing_peerid TEXT,
  info JSONB NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS naive.providers_info_log (
  provider_id INTEGER NOT NULL REFERENCES naive.providers ( provider_id ),
  info_entry_created TIMESTAMP WITH TIME ZONE NOT NULL,
  info_dialing_took_msecs INTEGER,
  info_dialing_peerid TEXT,
  info JSONB NOT NULL DEFAULT '{}'
);
CREATE OR REPLACE
  FUNCTION naive.record_provider_info_change() RETURNS TRIGGER
    LANGUAGE plpgsql
AS $$
BEGIN
  INSERT INTO naive.providers_info_log (
    provider_id, info_entry_created, info_dialing_took_msecs, info_dialing_peerid, info
  ) VALUES(
    NEW.provider_id, NEW.provider_last_polled, NEW.info_dialing_took_msecs, NEW.info_dialing_peerid, NEW.info
  );
  UPDATE naive.providers_info SET
    info_last_updated = NEW.provider_last_polled
  WHERE provider_id = NEW.provider_id;
  RETURN NULL;
END;
$$;
CREATE OR REPLACE TRIGGER trigger_new_provider_info
  AFTER INSERT ON naive.providers_info
  FOR EACH ROW
  EXECUTE PROCEDURE naive.record_provider_info_change()
;
CREATE OR REPLACE TRIGGER trigger_update_provider_info
  AFTER UPDATE ON naive.providers_info
  FOR EACH ROW
  WHEN ( OLD.info != NEW.info )
  EXECUTE PROCEDURE naive.record_provider_info_change()
;


CREATE TABLE IF NOT EXISTS naive.published_deals (
  deal_id BIGINT UNIQUE NOT NULL CONSTRAINT deal_valid_id CHECK ( deal_id > 0 ),
  piece_id BIGINT NOT NULL REFERENCES naive.pieces ( piece_id ) ON UPDATE CASCADE,
  provider_id INTEGER NOT NULL REFERENCES naive.providers ( provider_id ),
  client_id INTEGER NOT NULL REFERENCES naive.clients ( client_id ),
  claimed_log2_size SMALLINT NOT NULL CONSTRAINT piece_valid_size CHECK ( claimed_log2_size > 0 ),
  state SMALLINT NOT NULL CONSTRAINT deal_valid_state CHECK ( state IN ( 1, 3, 8 ) ), -- see status GENERATED below
  label BYTEA NOT NULL,
  decoded_label TEXT CONSTRAINT deal_valid_label_cid CHECK ( naive.looks_like_cid( decoded_label ) ),
  is_filplus BOOL NOT NULL,
  status TEXT NOT NULL GENERATED ALWAYS AS ( CASE state
    WHEN 1 THEN 'published'
    WHEN 3 THEN 'active'
    WHEN 8 THEN 'terminated'
  END ) STORED,
  published_deal_meta JSONB NOT NULL DEFAULT '{}',
  start_epoch INTEGER NOT NULL CONSTRAINT deal_valid_start CHECK ( start_epoch > 0 ),
  end_epoch INTEGER NOT NULL CONSTRAINT deal_valid_end CHECK ( end_epoch > 0 ),
  sector_start_epoch INTEGER CONSTRAINT deal_valid_sector_start CHECK ( sector_start_epoch > 0 ),
  sector_start_rounded INTEGER GENERATED ALWAYS AS ( ( sector_start_epoch - 240 ) / 2880 * 2880 + 240 ) STORED, -- 2h +/- because network started at 22:00 UTC
  entry_created TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS published_deals_piece_id_idx ON naive.published_deals ( piece_id );
CREATE INDEX IF NOT EXISTS published_deals_status ON naive.published_deals ( status, piece_id, is_filplus, provider_id );
CREATE INDEX IF NOT EXISTS published_deals_state ON naive.published_deals ( state, provider_id ) INCLUDE ( piece_id );
CREATE INDEX IF NOT EXISTS published_deals_live ON naive.published_deals ( piece_id ) WHERE ( state < 8 );
CREATE OR REPLACE
  FUNCTION naive.init_deal_relations() RETURNS TRIGGER
    LANGUAGE plpgsql
AS $$
BEGIN
  INSERT INTO naive.clients( client_id ) VALUES ( NEW.client_id ) ON CONFLICT DO NOTHING;
  INSERT INTO naive.providers( provider_id ) VALUES ( NEW.provider_id ) ON CONFLICT DO NOTHING;
  RETURN NEW;
 END;
$$;
CREATE OR REPLACE TRIGGER trigger_init_deal_relations
  BEFORE INSERT ON naive.published_deals
  FOR EACH ROW
  EXECUTE PROCEDURE naive.init_deal_relations()
;

CREATE OR REPLACE VIEW naive.storage_stats AS (
  WITH
    private_sps AS MATERIALIZED (
      SELECT provider_id FROM naive.providers_info WHERE info_dialing_peerid IS NULL
    ),
    total AS (
      SELECT(
        (
          SELECT JSONB_BUILD_OBJECT(
            'total_unique_clients', COUNT( DISTINCT( client_id )),
            'total_unique_providers', COUNT( DISTINCT( provider_id )),
            'total_num_deals', COUNT(*),
            'total_stored_data_size', SUM( 1::BIGINT<<claimed_log2_size ),
            'total_fraction_of_raw_capacity', (
              ( SUM( 1::BIGINT<<claimed_log2_size )::NUMERIC )
                /
              ( SELECT (metadata->'market_state'->>'total_raw_capacity' )::NUMERIC FROM naive.global )
            )::NUMERIC( 10, 9 )
          ) FROM naive.published_deals WHERE state = 3
        )
          ||
        (
          SELECT JSONB_BUILD_OBJECT(
            'total_unique_cids', COUNT( piece_id ),
            'total_unique_data_size', SUM( 1::BIGINT<<proven_log2_size )
          ) FROM naive.pieces WHERE piece_id IN ( SELECT piece_id FROM naive.published_deals WHERE state = 3 )
        )
      ) AS summary
    ),
    total_filp AS (
      SELECT(
        (
          SELECT JSONB_BUILD_OBJECT(
            'total_unique_clients', COUNT( DISTINCT( client_id )),
            'total_unique_providers', COUNT( DISTINCT( provider_id )),
            'total_num_deals', COUNT(*),
            'total_stored_data_size', SUM( 1::BIGINT<<claimed_log2_size ),
            'total_fraction_of_raw_capacity', (
              ( SUM( 1::BIGINT<<claimed_log2_size )::NUMERIC )
                /
              ( SELECT (metadata->'market_state'->>'total_raw_capacity' )::NUMERIC FROM naive.global )
            )::NUMERIC( 10, 9 )
          ) FROM naive.published_deals WHERE state = 3 AND is_filplus
        )
          ||
        (
          SELECT JSONB_BUILD_OBJECT(
            'total_unique_cids', COUNT( piece_id ),
            'total_unique_data_size', SUM( 1::BIGINT<<proven_log2_size )
          ) FROM naive.pieces WHERE piece_id IN ( SELECT piece_id FROM naive.published_deals WHERE state = 3 AND is_filplus )
        )
      ) AS summary
    ),
    total_filp_private AS (
      SELECT(
        (
          SELECT JSONB_BUILD_OBJECT(
            'total_unique_clients', COUNT( DISTINCT( client_id )),
            'total_unique_providers', COUNT( DISTINCT( provider_id )),
            'total_num_deals', COUNT(*),
            'total_stored_data_size', SUM( 1::BIGINT<<claimed_log2_size ),
            'total_fraction_of_raw_capacity', (
              ( SUM( 1::BIGINT<<claimed_log2_size )::NUMERIC )
                /
              ( SELECT (metadata->'market_state'->>'total_raw_capacity' )::NUMERIC FROM naive.global )
            )::NUMERIC( 10, 9 )

          ) FROM naive.published_deals JOIN private_sps USING (provider_id) WHERE state = 3 AND is_filplus
        )
          ||
        (
          SELECT JSONB_BUILD_OBJECT(
            'total_unique_cids', COUNT( piece_id ),
            'total_unique_data_size', SUM( 1::BIGINT<<proven_log2_size )
          ) FROM naive.pieces WHERE piece_id IN ( SELECT piece_id FROM naive.published_deals JOIN private_sps USING (provider_id) WHERE state = 3 AND is_filplus )
        )
      ) AS summary
    ),
    total_private AS (
      SELECT(
        (
          SELECT JSONB_BUILD_OBJECT(
            'total_unique_clients', COUNT( DISTINCT( client_id )),
            'total_unique_providers', COUNT( DISTINCT( provider_id )),
            'total_num_deals', COUNT(*),
            'total_stored_data_size', SUM( 1::BIGINT<<claimed_log2_size ),
            'total_fraction_of_raw_capacity', (
              ( SUM( 1::BIGINT<<claimed_log2_size )::NUMERIC )
                /
              ( SELECT (metadata->'market_state'->>'total_raw_capacity' )::NUMERIC FROM naive.global )
            )::NUMERIC( 10, 9 )
          ) FROM naive.published_deals JOIN private_sps USING (provider_id) WHERE state = 3
        )
          ||
        (
          SELECT JSONB_BUILD_OBJECT(
            'total_unique_cids', COUNT( piece_id ),
            'total_unique_data_size', SUM( 1::BIGINT<<proven_log2_size )
          ) FROM naive.pieces WHERE piece_id IN ( SELECT piece_id FROM naive.published_deals JOIN private_sps USING (provider_id) WHERE state = 3 )
        )
      ) AS summary
    ),
    state AS (
      SELECT
        (metadata->'market_state'->'epoch')::INTEGER AS epoch,
        metadata->'market_state'->>'total_raw_capacity' AS total_raw,
        metadata->'market_state'->>'total_qa_power' AS total_qap
      FROM naive.global
    )
  SELECT JSONB_BUILD_OBJECT(
    'epoch', state.epoch,
    'timestamp', naive.ts_from_epoch( state.epoch ),
    'endpoint', 'NETWORK_WIDE_TOTALS',
    'payload', total.summary || JSONB_BUILD_OBJECT(
      'total_qa_power', state.total_qap,
      'total_raw_capacity', state.total_raw,
      'filplus_subset', total_filp.summary,
      'filplus_private_subset', total_filp_private.summary,
      'private_subset', total_private.summary
    )
  ) AS stats
    FROM state, total, total_filp, total_filp_private, total_private
);
