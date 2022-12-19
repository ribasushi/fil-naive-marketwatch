fil-naive-marketwatch
==================

This repository contains the software powering some of the DataPrograms
internal oracles. The author does not recommend anyone run this:
it's [not good enough](https://youtu.be/lN-KHmHz5NU).

The current [ERD can be found here](https://raw.githubusercontent.com/ribasushi/fil-naive-marketwatch/master/misc/pg_schema_diagram.svg)

Some notes on data model in no particular order:

- All data is present in a separate schema `naive`: for comfort you can

   `SET SEARCH_PATH = naive;`

- For performance all `f0xxx` _"ID"_ type addresses are represented as integers ( the `f0` part is dropped )
- For the same reason sizes are represented as powers of two: for stats use something like:

  `SELECT PG_SIZE_PRETTY ( SUM ( 1::BIGINT << proven_log2_size ) ) FROM pieces;`

- The source state/epoch are captured within the `global` table:

  `SELECT ts_from_epoch( ( metadata->'market_state'->'epoch' )::INTEGER ) FROM global;`

- Deal lifecycles stored in `published_deals.state`/`published_deals.status` go in one direction only:

  `(1)published` => (ideally) `(3)active` => `(8)terminated`

- `providers_info` contains the results of randomly polling a subset of providers that have ever made a deal. Dig through the `info` JSONB for various interesting stats.

- `providers_info_log` contains all historic changes since the poller has been operating

- Do not use `entry_created`: it merely reflects when the chain was polled. Instead `sector_start_epoch` contains the exact epoch at which a sector was deemed `active`.

- For easier aggregation column `sector_start_rounded` is generated from the above, which you can use like:

  ```
  WITH per_client_per_day AS (
    SELECT
        sector_start_rounded,
        client_id,
        COUNT( * ) AS daily_count,
        SUM( ( CASE WHEN is_filplus THEN 10 ELSE 1 END ) * (1::BIGINT << claimed_log2_size) ) AS daily_qap_added
      FROM published_deals
    WHERE sector_start_rounded IS NOT NULL
    GROUP BY sector_start_rounded, client_id
  )
  SELECT
      ts_from_epoch( sector_start_rounded ),
      client_id,
      client_address,
      daily_count,
      PG_SIZE_PRETTY( daily_qap_added ) AS daily_qap_added
    FROM per_client_per_day
    JOIN clients USING ( client_id )
  ORDER BY sector_start_rounded, client_address
  ;
  ```

- Not everything has indexes - it is trivial to add them though
