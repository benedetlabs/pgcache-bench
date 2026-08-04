-- ─────────────────────────────────────────────────────────────────────────────
-- Minimal schema for the R1 / R1b spike.
--
-- Shaped like the two Strapi structures that carry the read path (STUDY §6
-- "COPY inventory", §4.1 relation table), reduced to the smallest thing that
-- still exercises the same wire behaviour:
--
--   articles           -- the content table: integer `increments` PK + text cols
--   articles_tags_lnk  -- the join table: integer PK + two FK-shaped columns
--                         + a float order column (STUDY §6: order columns are
--                         `float`, `metadata/relations.ts:543-548`)
--
-- BOTH tables carry a primary key. PgCache cannot cache PK-less tables, and
-- STUDY §1 records that Strapi has none ("no PK-less tables",
-- `metadata/relations.ts:257-259,471-473`) — so a PK-less spike table would
-- test a shape Strapi never produces and would fail for the wrong reason.
--
-- Deliberately NOT modelled here: `tags`/`authors` target tables, draft/publish
-- row pairs, i18n columns, components. This spike answers a protocol question,
-- not a relational-fidelity question. `tag_id` is therefore a foreign-key-shaped
-- integer column with no target table and no constraint; `article_id` carries a
-- real FK so the join table is a genuine dependent relation.
--
-- Applied by run.sh BEFORE PgCache starts, so no DDL ever lands on a table that
-- is already inside PgCache's publication.
-- ─────────────────────────────────────────────────────────────────────────────

BEGIN;

DROP TABLE IF EXISTS articles_tags_lnk CASCADE;
DROP TABLE IF EXISTS articles CASCADE;

CREATE TABLE articles (
    id    integer PRIMARY KEY,
    title text    NOT NULL,
    slug  text    NOT NULL,
    views integer NOT NULL,
    body  text    NOT NULL
);

CREATE TABLE articles_tags_lnk (
    id         integer PRIMARY KEY,
    article_id integer NOT NULL REFERENCES articles (id) ON DELETE CASCADE,
    tag_id     integer NOT NULL,
    tag_ord    double precision
);

CREATE INDEX articles_tags_lnk_article_fk ON articles_tags_lnk (article_id);
CREATE INDEX articles_tags_lnk_tag_fk     ON articles_tags_lnk (tag_id);

-- 60 articles. Every column is a pure function of `i`, in the spirit of the
-- §4.1 analytical oracle, so the client can assert exact expected rows without
-- a second query.
--   title/slug : 'art-{i}'
--   views      : (i * 2654435761) mod 10^6   (§4.1 generator; bigint to avoid
--                                             integer overflow)
--   body       : fixed 512-byte filler, well under the TOAST threshold
INSERT INTO articles (id, title, slug, views, body)
SELECT i,
       'art-' || i,
       'art-' || i,
       ((i::bigint * 2654435761) % 1000000)::int,
       repeat('x', 512)
FROM generate_series(1, 60) AS g(i);

-- 3 link rows per article = 180 rows, dense tag_ord 1..3 per parent
-- (STUDY §6 "Order columns are float ... dense integers from 1 is a seeding
-- convention chosen to match what the app would have written").
INSERT INTO articles_tags_lnk (id, article_id, tag_id, tag_ord)
SELECT (a.i - 1) * 3 + t.k,
       a.i,
       ((a.i * 7 + t.k) % 10) + 1,
       t.k::double precision
FROM generate_series(1, 60) AS a(i),
     generate_series(1, 3)  AS t(k);

COMMIT;

ANALYZE articles;
ANALYZE articles_tags_lnk;

-- Guard: the client's expected-row assertions assume exactly this mass.
DO $$
DECLARE n_art int; n_lnk int;
BEGIN
    SELECT count(*) INTO n_art FROM articles;
    SELECT count(*) INTO n_lnk FROM articles_tags_lnk;
    IF n_art <> 60 OR n_lnk <> 180 THEN
        RAISE EXCEPTION 'spike seed wrong: articles=% (want 60), links=% (want 180)', n_art, n_lnk;
    END IF;
END $$;
