WITH types AS (
 SELECT * FROM pg_catalog.pg_type WHERE oid=ANY(ARRAY[16,17,20,21,23,25,114,700,701,1042,1043,1082,1083,1114,1184,1700,2950,3802]::oid[])
), ops AS (
 SELECT o.* FROM pg_catalog.pg_operator o WHERE o.oprnamespace=11
 AND ((o.oprleft IN (SELECT oid FROM types) AND o.oprright=o.oprleft AND o.oprname IN ('=','<>','<','<=','>','>=','+','-','*','/'))
 OR (o.oprleft=0 AND o.oprright IN (20,21,23,700,701,1700) AND o.oprname='-'))
), aggs AS (
 SELECT a.* FROM pg_catalog.pg_aggregate a JOIN pg_catalog.pg_proc p ON p.oid=a.aggfnoid
 WHERE p.pronamespace=11 AND ((p.proname IN ('sum','avg','min','max') AND p.pronargs=1 AND p.proargtypes[0] IN (SELECT oid FROM types)) OR p.proname='count')
), casts AS (
 SELECT * FROM pg_catalog.pg_cast WHERE castsource IN (20,21,23,700,701,1700) AND casttarget IN (20,21,23,700,701,1700)
), classes AS (
 SELECT o.* FROM pg_catalog.pg_opclass o WHERE o.opcmethod=403 AND o.opcnamespace=11 AND o.opcintype IN (SELECT oid FROM types)
), members AS (
 SELECT m.* FROM pg_catalog.pg_amop m WHERE m.amopfamily IN (SELECT opcfamily FROM classes)
), supports AS (
 SELECT p.* FROM pg_catalog.pg_amproc p WHERE p.amprocfamily IN (SELECT opcfamily FROM classes)
), procs AS (
 SELECT p.* FROM pg_catalog.pg_proc p WHERE p.oid IN (
 SELECT oprcode FROM ops UNION SELECT t.typinput FROM types t UNION SELECT t.typoutput FROM types t
 UNION SELECT t.typreceive FROM types t UNION SELECT t.typsend FROM types t
 UNION SELECT castfunc FROM casts UNION SELECT amproc FROM supports
 UNION SELECT aggfnoid FROM aggs UNION SELECT aggtransfn FROM aggs UNION SELECT aggfinalfn FROM aggs
 UNION SELECT aggcombinefn FROM aggs UNION SELECT aggserialfn FROM aggs UNION SELECT aggdeserialfn FROM aggs
 UNION SELECT aggmtransfn FROM aggs UNION SELECT aggminvtransfn FROM aggs UNION SELECT aggmfinalfn FROM aggs
 UNION SELECT o.oprcode FROM pg_catalog.pg_operator o JOIN members m ON m.amopopr=o.oid)
)
SELECT pg_catalog.jsonb_build_object(
 'types',(SELECT pg_catalog.jsonb_agg(pg_catalog.to_jsonb(t)-'typowner'-'typacl' ORDER BY oid) FROM types t),
 'operators',(SELECT pg_catalog.jsonb_agg(pg_catalog.to_jsonb(o)-'oprowner' ORDER BY oid) FROM ops o),
 'aggregates',(SELECT pg_catalog.jsonb_agg(pg_catalog.to_jsonb(a) ORDER BY aggfnoid::oid) FROM aggs a),
 'casts',(SELECT pg_catalog.jsonb_agg(pg_catalog.to_jsonb(c) ORDER BY oid) FROM casts c),
 'classes',(SELECT pg_catalog.jsonb_agg(pg_catalog.to_jsonb(c)-'opcowner' ORDER BY oid) FROM classes c),
 'members',(SELECT pg_catalog.jsonb_agg(pg_catalog.to_jsonb(m) ORDER BY oid) FROM members m),
 'supports',(SELECT pg_catalog.jsonb_agg(pg_catalog.to_jsonb(s) ORDER BY oid) FROM supports s),
 'functions',(SELECT pg_catalog.jsonb_agg(pg_catalog.to_jsonb(p)-'proowner'-'proacl' ORDER BY oid) FROM procs p)
)::text
