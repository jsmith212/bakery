-- Nothing references these tables -- a unihash is a GC root, not a referent -- so the
-- two drops are independent and their indexes go with them. The FKs point OUTWARD, at
-- cache_backends, and dropping the referencing side never troubles a RESTRICT.
DROP TABLE IF EXISTS hashserv_outhashes;
DROP TABLE IF EXISTS hashserv_unihashes;
